package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/exp/slices"
	"gopkg.in/yaml.v3"

	consolelog "github.com/kyma-project/test-infra/pkg/logging"
	"github.com/kyma-project/test-infra/pkg/prow/externalplugin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/test-infra/prow/github"
)

type githubClient interface {
	CreatePullRequestReviewComment(org, repo string, number int, rc github.ReviewComment) error
	CreateReview(org, repo string, number int, r github.DraftReview) error
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
	AddLabel(org, repo string, number int, label string) error
	CreateComment(org, repo string, number int, comment string) error
}

// handlerBackend is a backend for the plugin.
// It contains all the configuration and clients needed to handle events.
type handlerBackend struct {
	ghc                    githubClient
	logLevel               zapcore.Level
	waitForStatusesTimeout int                                                         // in seconds
	rulesPath              string                                                      // Path to yaml config file
	conditions             map[string]map[string]map[string][]ApproveCondition         `yaml:"conditions"`
	prLocks                map[string]map[string]map[int]map[string]context.CancelFunc // Holds head sha and cancel function of PRs that are being processed. org -> repo -> pr number -> head sha -> cancel function
	prMutex                sync.Mutex
}

// WatchConfig watches for changes in config file and reloads it.
func (hb *handlerBackend) watchConfig(logger *zap.SugaredLogger) {
	defer logger.Sync()
	logger.Info("Starting config watcher")
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatal("NewWatcher failed: ", err)
	}
	defer watcher.Close()

	done := make(chan bool)
	go func() {
		defer close(done)

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				logger.Infof("%s %s", event.Name, event.Op)
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					logger.Info("Reloading config")
					err := hb.readConfig()
					if err != nil {
						logger.Fatalf("Failed reading config: %s", err)
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logger.Errorf("error: %s", err)
			}
		}

	}()

	err = watcher.Add(hb.rulesPath)
	if err != nil {
		logger.Fatalf("Add failed: %s", err)
	}
	<-done
}

// lockPR locks PR for processing by adding head sha to prLocks.
// If PR is already locked, returns false.
// Because GitHub sends multiple review request events for one PR, we need to lock PR to avoid processing it multiple times.
// GitHub sends multiple events because it sends one event for each reviewer.
func (hb *handlerBackend) lockPR(cancel context.CancelFunc, logger *zap.SugaredLogger, org, repo, headSha string, prNumber int) bool {
	// Sync access to prLocks with mutex.
	hb.prMutex.Lock()
	defer hb.prMutex.Unlock()
	defer logger.Sync()
	_, ok := hb.prLocks[org][repo][prNumber][headSha]
	if !ok {
		if hb.prLocks[org] == nil {
			hb.prLocks[org] = make(map[string]map[int]map[string]context.CancelFunc)
		}
		if hb.prLocks[org][repo] == nil {
			hb.prLocks[org][repo] = make(map[int]map[string]context.CancelFunc)
		}
		if hb.prLocks[org][repo][prNumber] == nil {
			hb.prLocks[org][repo][prNumber] = make(map[string]context.CancelFunc)
		}
		hb.prLocks[org][repo][prNumber][headSha] = cancel
		return true
	}
	return false
}

// unlockPR unlocks PR by removing head sha from prLocks.
func (hb *handlerBackend) unlockPR(logger *zap.SugaredLogger, org, repo, headSha string, prNumber int) {
	// Sync access to prLocks with mutex.
	hb.prMutex.Lock()
	defer hb.prMutex.Unlock()
	defer logger.Sync()
	delete(hb.prLocks[org][repo][prNumber], headSha)
	if len(hb.prLocks[org][repo][prNumber]) == 0 {
		delete(hb.prLocks[org][repo], prNumber)
	}
}

// cancelPR cancels processing of PR by head sha. It calls cancel function assigned to head sha in prLocks.
func (hb *handlerBackend) cancelPR(logger *zap.SugaredLogger, org, repo, headSha string, prNumber int) {
	// Sync access to prLocks with mutex.
	hb.prMutex.Lock()
	defer hb.prMutex.Unlock()
	defer logger.Sync()
	if pr, ok := hb.prLocks[org][repo][prNumber]; ok {
		for sha, cancel := range pr {
			if sha != headSha {
				cancel()
			}
		}
	}
}

// ApproveCondition defines conditions for approving PR.
type ApproveCondition struct {
	RequiredLabels []string `yaml:"requiredLabels"`
	ChangedFiles   []string `yaml:"changedFiles"`
}

// String returns string representation of ApproveCondition.
func (ac *ApproveCondition) String() string {
	b, _ := json.Marshal(ac)
	return string(b)
}

// checkRequiredLabels checks if PR has all required labels.
func (ac *ApproveCondition) checkRequiredLabels(logger *zap.SugaredLogger, prLabels []github.Label) bool {
	defer logger.Sync()
	if ac.RequiredLabels == nil {
		logger.Debug("No required labels defined")
		// No required labels defined
		return true
	}
	pl := make(map[string]interface{})
	logger.Debugf("Checking if PR has all required labels: %v", ac.RequiredLabels)
	for _, l := range prLabels {
		pl[l.Name] = nil
	}
	for _, requiredLabel := range ac.RequiredLabels {
		if _, ok := pl[requiredLabel]; !ok {
			logger.Debugf("PR is missing required label: %s", requiredLabel)
			return false
		}
	}
	logger.Debug("All required labels are present")
	return true
}

// checkChangedFiles checks if PR changed only allowed files.
func (ac *ApproveCondition) checkChangedFiles(logger *zap.SugaredLogger, changes []github.PullRequestChange) bool {
	defer logger.Sync()
	logger.Debugf("Checking if PR changed only allowed files: %v", ac.ChangedFiles)
	for _, change := range changes {
		change := change
		logger.Debugf("Checking file: %s", change.Filename)
		matched := slices.ContainsFunc(ac.ChangedFiles, func(allowedFile string) bool {
			filesMatcher := regexp.MustCompile(allowedFile)
			matched := filesMatcher.MatchString(change.Filename)
			logger.Debugf("File %s matched %s: %t", change.Filename, allowedFile, matched)
			return matched
		})
		if !matched {
			logger.Debugf("File %s not matched", change.Filename)
			return false
		}
	}
	logger.Debug("All files matched")
	return true
}

// readConfig reads config from config file.
func (hb *handlerBackend) readConfig() error {
	c := make(map[string]map[string]map[string]map[string][]ApproveCondition)
	configFile, err := os.ReadFile(hb.rulesPath)
	if err == nil {
		yaml.Unmarshal(configFile, &c)
		hb.conditions = c["conditions"]
		return nil
	}
	return err
}

// checkPrStatuses checks if all statuses are in success state.
// Tide required status check is not taken into account. It will be always pending until PR is ready to merge.
// Timeout limits time waiting for statuses became success.
func (hb *handlerBackend) checkPrStatuses(ctx context.Context, logger *zap.SugaredLogger, prOrg, prRepo, prHeadSha string, prNumber int) error {
	defer logger.Sync()
	// Sleep for 30 seconds to make sure all statuses are registered.
	logger.Debug("Sleeping for 30 seconds to make sure all statuses are registered")
	time.Sleep(30 * time.Second)

	backOff := backoff.NewExponentialBackOff()
	backOff.MaxElapsedTime = time.Duration(hb.waitForStatusesTimeout) * time.Second
	backOff.MaxInterval = 10 * time.Minute
	backOff.InitialInterval = 5 * time.Minute
	logger.Debugf("Waiting for statuses to become success. Timeout: %d", hb.waitForStatusesTimeout)

	// Check if context canceled in function to not process PR if it was canceled.
	err := backoff.Retry(func() error {
		select {
		case <-ctx.Done():
			return backoff.Permanent(ctx.Err())
		default:
			defer logger.Sync()
			prStatuses, err := hb.ghc.GetCombinedStatus(prOrg, prRepo, prHeadSha)
			if err != nil {
				gherr := fmt.Errorf("failed get pull request contexts combined status, got error %w", err)
				logger.Error(gherr.Error())
				return gherr
			}
			// Don't check if pr checks status is success as that means all context are success, even tide context.
			// That means a pr was already approved and is ready for merge, because tide context transition to success
			// when pr is ready for merge.
			logger.Debugf("Pull request %d status: %s", prNumber, prStatuses.State)
			switch prState := prStatuses.State; prState {
			case "failure":
				return backoff.Permanent(fmt.Errorf("pull request %d is in failure state, skip approving", prNumber))
			case "pending":
				logger.Infof("Pull request %d is in pending state, wait for statuses to become success.", prNumber)
				for _, prStatus := range prStatuses.Statuses {
					if prStatus.State == "failure" {
						return backoff.Permanent(fmt.Errorf("pull request status check %s failed", prStatus.Context))
					} else if prStatus.State == "pending" && prStatus.Context != "tide" {
						statusErr := fmt.Errorf("pull request status check %s is pending", prStatus.Context)
						logger.Debug(statusErr.Error())
						return statusErr
					}
				}
			}
			logger.Debugf("All statuses are success")
			return nil
		}
	}, backOff)
	return err
}

func (hb *handlerBackend) checkPrApproveConditions(logger *zap.SugaredLogger, conditions []ApproveCondition, changes []github.PullRequestChange, prLabels []github.Label) bool {
	defer logger.Sync()
	for _, condition := range conditions {
		logger.Debugw("Checking condition", "condition", condition)
		labelsMatched := condition.checkRequiredLabels(logger, prLabels)
		if !labelsMatched {
			logger.Debug("Labels not matched")
			continue
		}
		filesMatched := condition.checkChangedFiles(logger, changes)
		if !filesMatched {
			logger.Debug("Files not matched")
			continue
		}
		return true
	}
	logger.Debug("No conditions matched")
	return false
}

func (hb *handlerBackend) reviewPullRequest(ctx context.Context, logger *zap.SugaredLogger, prOrg, prRepo, prUser, prHeadSha string, prNumber int, prLabels []github.Label) {
	defer logger.Sync()
	defer hb.unlockPR(logger, prOrg, prRepo, prHeadSha, prNumber)
	logger.Debugf("Checking if conditions for PR author %s exists: %t", prUser, hb.conditions[prOrg][prRepo][prUser] != nil)
	if conditions, ok := hb.conditions[prOrg][prRepo][prUser]; ok {
		logger.Debugf("Checking if PR %d meets approval conditions: %v", prNumber, conditions)

		// Get changes from pull request.
		changes, err := hb.ghc.GetPullRequestChanges(prOrg, prRepo, prNumber)
		if err != nil {
			logger.Errorw("failed get pull request changes", "error", err.Error())
		}
		logger.Sync() // Syncing logger to make sure all logs from calling GitHub API are written before logs from functions called in next steps.
		conditionsMatched := hb.checkPrApproveConditions(logger, conditions, changes, prLabels)
		if !conditionsMatched {
			return
		}
		err = hb.checkPrStatuses(ctx, logger, prOrg, prRepo, prHeadSha, prNumber)
		if err != nil {
			logger.Errorf("pull request %s/%s#%d has non success statuses, got error: %s",
				prOrg,
				prRepo,
				prNumber,
				err)
			return
		}
		// Check if context canceled to not review commit which is not a HEAD anymore.
		select {
		case <-ctx.Done():
			logger.Infof("Context canceled, skip approving pull request %s/%s#%d", prOrg, prRepo, prNumber)
			return
		default:
			review := github.DraftReview{
				CommitSHA: prHeadSha,
				Body:      "",
				Action:    "APPROVE",
				Comments:  nil,
			}
			err = hb.ghc.CreateReview(prOrg, prRepo, prNumber, review)
			if err != nil {
				logger.Errorf("failed create review for pull request %s/%s#%d sha: %s, got error: %s",
					prOrg,
					prRepo,
					prNumber,
					prHeadSha,
					err)
				return
			}
			logger.Infof("Pull request %s/%s#%d was approved.", prOrg, prRepo, prNumber)
			err = hb.ghc.AddLabel(prOrg, prRepo, prNumber, "auto-approved")
			if err != nil {
				logger.Errorf("failed add label to pull request %s/%s#%d, got error: %s",
					prOrg,
					prRepo,
					prNumber,
					err)
			}
			logger.Infof("Label auto-approved was added to pull request %s/%s#%d.", prOrg, prRepo, prNumber)
		}
	} else {
		logger.Infof("Pull request %s/%s#%d doesn't meet conditions to be auto approved, pr author %s doesn't have conditions defined.",
			prOrg,
			prRepo,
			prNumber,
			prUser)
	}
}

func (hb *handlerBackend) handleReviewRequestedAction(ctx context.Context, cancel context.CancelFunc, logger *zap.SugaredLogger, prEvent github.PullRequestEvent) {
	if locked := hb.lockPR(cancel, logger, prEvent.Repo.Owner.Login, prEvent.Repo.Name, prEvent.PullRequest.Head.SHA, prEvent.PullRequest.Number); !locked {
		logger.Infof("Reeview request for pull request head sha %s already in process.", prEvent.PullRequest.Head.SHA)
		return
	}
	logger.Debug("Got pull request review requested action")
	logger.Sync()
	hb.reviewPullRequest(ctx, logger, prEvent.Repo.Owner.Login, prEvent.Repo.Name, prEvent.PullRequest.User.Login, prEvent.PullRequest.Head.SHA, prEvent.PullRequest.Number, prEvent.PullRequest.Labels)
}

func (hb *handlerBackend) handlePrSynchronizeAction(ctx context.Context, cancel context.CancelFunc, logger *zap.SugaredLogger, prEvent github.PullRequestEvent) {
	// Cancel context for review for previous commit.
	hb.cancelPR(logger, prEvent.Repo.Owner.Login, prEvent.Repo.Name, prEvent.PullRequest.Head.SHA, prEvent.PullRequest.Number)
	if locked := hb.lockPR(cancel, logger, prEvent.Repo.Owner.Login, prEvent.Repo.Name, prEvent.PullRequest.Head.SHA, prEvent.PullRequest.Number); !locked {
		logger.Infof("Pull request head sha %s already in process.", prEvent.PullRequest.Head.SHA)
		return
	}
	logger.Debug("Got pull request synchronize action")
	logger.Sync()
	hb.reviewPullRequest(ctx, logger, prEvent.Repo.Owner.Login, prEvent.Repo.Name, prEvent.PullRequest.User.Login, prEvent.PullRequest.Head.SHA, prEvent.PullRequest.Number, prEvent.PullRequest.Labels)
}

func (hb *handlerBackend) handleReviewDismissedAction(ctx context.Context, cancel context.CancelFunc, logger *zap.SugaredLogger, reviewEvent github.ReviewEvent) {
	if locked := hb.lockPR(cancel, logger, reviewEvent.Repo.Owner.Login, reviewEvent.Repo.Name, reviewEvent.PullRequest.Head.SHA, reviewEvent.PullRequest.Number); !locked {
		logger.Infof("Pull request head sha %s already in process.", reviewEvent.PullRequest.Head.SHA)
		return
	}
	logger.Debug("Got pull request review dismissed action")
	logger.Sync()
	hb.reviewPullRequest(ctx, logger, reviewEvent.Repo.Owner.Login, reviewEvent.Repo.Name, reviewEvent.PullRequest.User.Login, reviewEvent.PullRequest.Head.SHA, reviewEvent.PullRequest.Number, reviewEvent.PullRequest.Labels)
}

func (hb *handlerBackend) pullRequestEventHandler(_ *externalplugin.Plugin, payload externalplugin.Event) {
	logger, atom := consolelog.NewLoggerWithLevel()
	defer logger.Sync()
	atom.SetLevel(hb.logLevel)
	logger = logger.With(externalplugin.EventTypeField, payload.EventType, github.EventGUID, payload.EventGUID)

	logger.Debug("Got pull_request payload")
	var prEvent github.PullRequestEvent
	if err := json.Unmarshal(payload.Payload, &prEvent); err != nil {
		logger.Errorw("Failed unmarshal json payload.", "error", err)
		return
	}
	logger = logger.With("pr-number", prEvent.Number)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	switch prEvent.Action {
	case github.PullRequestActionReviewRequested:
		hb.handleReviewRequestedAction(ctx, cancel, logger, prEvent)
	case github.PullRequestActionSynchronize:
		hb.handlePrSynchronizeAction(ctx, cancel, logger, prEvent)
	}
}

func (hb *handlerBackend) pullRequestReviewEventHandler(_ *externalplugin.Plugin, payload externalplugin.Event) {
	logger, atom := consolelog.NewLoggerWithLevel()
	defer logger.Sync()
	atom.SetLevel(hb.logLevel)
	logger = logger.With(externalplugin.EventTypeField, payload.EventType, github.EventGUID, payload.EventGUID)

	logger.Debug("Got pull_request_review payload")
	var reviewEvent github.ReviewEvent
	if err := json.Unmarshal(payload.Payload, &reviewEvent); err != nil {
		logger.Errorw("Failed unmarshal json payload.", "error", err)
		return
	}
	logger = logger.With("pr-number", reviewEvent.PullRequest.Number)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	switch reviewEvent.Action {
	case github.ReviewActionDismissed:
		hb.handleReviewDismissedAction(ctx, cancel, logger, reviewEvent)
	}
}
