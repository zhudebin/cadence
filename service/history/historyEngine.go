// Copyright (c) 2017-2021 Uber Technologies, Inc.
// Portions of the Software are attributed to Copyright (c) 2021 Temporal Technologies Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pborman/uuid"
	"go.uber.org/cadence/.gen/go/cadence/workflowserviceclient"
	"go.uber.org/yarpc/yarpcerrors"

	"github.com/uber/cadence/client/admin"
	hc "github.com/uber/cadence/client/history"
	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/client"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/definition"
	ce "github.com/uber/cadence/common/errors"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
	cndc "github.com/uber/cadence/common/ndc"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/reconciliation/invariant"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/decision"
	"github.com/uber/cadence/service/history/engine"
	"github.com/uber/cadence/service/history/events"
	"github.com/uber/cadence/service/history/execution"
	"github.com/uber/cadence/service/history/failover"
	"github.com/uber/cadence/service/history/ndc"
	"github.com/uber/cadence/service/history/query"
	"github.com/uber/cadence/service/history/queue"
	"github.com/uber/cadence/service/history/replication"
	"github.com/uber/cadence/service/history/reset"
	"github.com/uber/cadence/service/history/shard"
	"github.com/uber/cadence/service/history/task"
	"github.com/uber/cadence/service/history/workflow"
	warchiver "github.com/uber/cadence/service/worker/archiver"
)

const (
	defaultQueryFirstDecisionTaskWaitTime = time.Second
	queryFirstDecisionTaskCheckInterval   = 200 * time.Millisecond
	replicationTimeout                    = 30 * time.Second
	contextLockTimeout                    = 500 * time.Millisecond

	// TerminateIfRunningReason reason for terminateIfRunning
	TerminateIfRunningReason = "TerminateIfRunning Policy"
	// TerminateIfRunningDetailsTemplate details template for terminateIfRunning
	TerminateIfRunningDetailsTemplate = "New runID: %s"
)

var (
	errDomainDeprecated = &types.BadRequestError{Message: "Domain is deprecated."}
)

type (
	historyEngineImpl struct {
		currentClusterName        string
		shard                     shard.Context
		timeSource                clock.TimeSource
		decisionHandler           decision.Handler
		clusterMetadata           cluster.Metadata
		historyV2Mgr              persistence.HistoryManager
		executionManager          persistence.ExecutionManager
		visibilityMgr             persistence.VisibilityManager
		txProcessor               queue.Processor
		timerProcessor            queue.Processor
		nDCReplicator             ndc.HistoryReplicator
		nDCActivityReplicator     ndc.ActivityReplicator
		historyEventNotifier      events.Notifier
		tokenSerializer           common.TaskTokenSerializer
		executionCache            *execution.Cache
		metricsClient             metrics.Client
		logger                    log.Logger
		throttledLogger           log.Logger
		config                    *config.Config
		archivalClient            warchiver.Client
		workflowResetter          reset.WorkflowResetter
		queueTaskProcessor        task.Processor
		replicationTaskProcessors []replication.TaskProcessor
		replicationAckManager     replication.TaskAckManager
		publicClient              workflowserviceclient.Interface
		eventsReapplier           ndc.EventsReapplier
		matchingClient            matching.Client
		rawMatchingClient         matching.Client
		clientChecker             client.VersionChecker
		replicationDLQHandler     replication.DLQHandler
		failoverMarkerNotifier    failover.MarkerNotifier
	}
)

var _ engine.Engine = (*historyEngineImpl)(nil)

var (
	// FailedWorkflowCloseState is a set of failed workflow close states, used for start workflow policy
	// for start workflow execution API
	FailedWorkflowCloseState = map[int]bool{
		persistence.WorkflowCloseStatusFailed:     true,
		persistence.WorkflowCloseStatusCanceled:   true,
		persistence.WorkflowCloseStatusTerminated: true,
		persistence.WorkflowCloseStatusTimedOut:   true,
	}
)

// NewEngineWithShardContext creates an instance of history engine
func NewEngineWithShardContext(
	shard shard.Context,
	visibilityMgr persistence.VisibilityManager,
	matching matching.Client,
	historyClient hc.Client,
	publicClient workflowserviceclient.Interface,
	historyEventNotifier events.Notifier,
	config *config.Config,
	replicationTaskFetchers replication.TaskFetchers,
	rawMatchingClient matching.Client,
	queueTaskProcessor task.Processor,
	failoverCoordinator failover.Coordinator,
) engine.Engine {
	currentClusterName := shard.GetService().GetClusterMetadata().GetCurrentClusterName()

	logger := shard.GetLogger()
	executionManager := shard.GetExecutionManager()
	historyV2Manager := shard.GetHistoryManager()
	executionCache := execution.NewCache(shard)
	failoverMarkerNotifier := failover.NewMarkerNotifier(shard, config, failoverCoordinator)
	historyEngImpl := &historyEngineImpl{
		currentClusterName:   currentClusterName,
		shard:                shard,
		clusterMetadata:      shard.GetClusterMetadata(),
		timeSource:           shard.GetTimeSource(),
		historyV2Mgr:         historyV2Manager,
		executionManager:     executionManager,
		visibilityMgr:        visibilityMgr,
		tokenSerializer:      common.NewJSONTaskTokenSerializer(),
		executionCache:       executionCache,
		logger:               logger.WithTags(tag.ComponentHistoryEngine),
		throttledLogger:      shard.GetThrottledLogger().WithTags(tag.ComponentHistoryEngine),
		metricsClient:        shard.GetMetricsClient(),
		historyEventNotifier: historyEventNotifier,
		config:               config,
		archivalClient: warchiver.NewClient(
			shard.GetMetricsClient(),
			logger,
			publicClient,
			shard.GetConfig().NumArchiveSystemWorkflows,
			shard.GetConfig().ArchiveRequestRPS,
			shard.GetService().GetArchiverProvider(),
		),
		workflowResetter: reset.NewWorkflowResetter(
			shard,
			executionCache,
			logger,
		),
		publicClient:           publicClient,
		matchingClient:         matching,
		rawMatchingClient:      rawMatchingClient,
		queueTaskProcessor:     queueTaskProcessor,
		clientChecker:          client.NewVersionChecker(),
		failoverMarkerNotifier: failoverMarkerNotifier,
		replicationAckManager: replication.NewTaskAckManager(
			shard,
			executionCache,
		),
	}
	historyEngImpl.decisionHandler = decision.NewHandler(
		shard,
		historyEngImpl.executionCache,
		historyEngImpl.tokenSerializer,
	)
	pRetry := persistence.NewPersistenceRetryer(
		shard.GetExecutionManager(),
		shard.GetHistoryManager(),
		common.CreatePersistenceRetryPolicy(),
	)
	openExecutionCheck := invariant.NewConcreteExecutionExists(pRetry)

	historyEngImpl.txProcessor = queue.NewTransferQueueProcessor(
		shard,
		historyEngImpl,
		queueTaskProcessor,
		executionCache,
		historyEngImpl.workflowResetter,
		historyEngImpl.archivalClient,
		openExecutionCheck,
	)

	historyEngImpl.timerProcessor = queue.NewTimerQueueProcessor(
		shard,
		historyEngImpl,
		queueTaskProcessor,
		executionCache,
		historyEngImpl.archivalClient,
		openExecutionCheck,
	)

	historyEngImpl.eventsReapplier = ndc.NewEventsReapplier(shard.GetMetricsClient(), logger)

	// Only start the replicator processor if global domain is enabled
	if shard.GetClusterMetadata().IsGlobalDomainEnabled() {
		historyEngImpl.nDCReplicator = ndc.NewHistoryReplicator(
			shard,
			executionCache,
			historyEngImpl.eventsReapplier,
			logger,
		)
		historyEngImpl.nDCActivityReplicator = ndc.NewActivityReplicator(
			shard,
			executionCache,
			logger,
		)
	}

	var replicationTaskProcessors []replication.TaskProcessor
	replicationTaskExecutors := make(map[string]replication.TaskExecutor)
	// Intentionally use the raw client to create its own retry policy
	historyRawClient := shard.GetService().GetClientBean().GetHistoryClient()
	historyRetryableClient := hc.NewRetryableClient(
		historyRawClient,
		common.CreateReplicationServiceBusyRetryPolicy(),
		common.IsServiceBusyError,
	)
	resendFunc := func(ctx context.Context, request *types.ReplicateEventsV2Request) error {
		return historyRetryableClient.ReplicateEventsV2(ctx, request)
	}
	for _, replicationTaskFetcher := range replicationTaskFetchers.GetFetchers() {
		sourceCluster := replicationTaskFetcher.GetSourceCluster()
		// Intentionally use the raw client to create its own retry policy
		adminClient := shard.GetService().GetClientBean().GetRemoteAdminClient(sourceCluster)
		adminRetryableClient := admin.NewRetryableClient(
			adminClient,
			common.CreateReplicationServiceBusyRetryPolicy(),
			common.IsServiceBusyError,
		)
		historyResender := cndc.NewHistoryResender(
			shard.GetDomainCache(),
			adminRetryableClient,
			resendFunc,
			shard.GetService().GetPayloadSerializer(),
			nil,
			openExecutionCheck,
			shard.GetLogger(),
		)
		replicationTaskExecutor := replication.NewTaskExecutor(
			shard,
			shard.GetDomainCache(),
			historyResender,
			historyEngImpl,
			shard.GetMetricsClient(),
			shard.GetLogger(),
		)
		replicationTaskExecutors[sourceCluster] = replicationTaskExecutor

		replicationTaskProcessor := replication.NewTaskProcessor(
			shard,
			historyEngImpl,
			config,
			shard.GetMetricsClient(),
			replicationTaskFetcher,
			replicationTaskExecutor,
		)
		replicationTaskProcessors = append(replicationTaskProcessors, replicationTaskProcessor)
	}
	historyEngImpl.replicationTaskProcessors = replicationTaskProcessors
	replicationMessageHandler := replication.NewDLQHandler(shard, replicationTaskExecutors)
	historyEngImpl.replicationDLQHandler = replicationMessageHandler

	shard.SetEngine(historyEngImpl)
	return historyEngImpl
}

// Start will spin up all the components needed to start serving this shard.
// Make sure all the components are loaded lazily so start can return immediately.  This is important because
// ShardController calls start sequentially for all the shards for a given host during startup.
func (e *historyEngineImpl) Start() {
	e.logger.Info("History engine state changed", tag.LifeCycleStarting)
	defer e.logger.Info("History engine state changed", tag.LifeCycleStarted)

	e.txProcessor.Start()
	e.timerProcessor.Start()

	// failover callback will try to create a failover queue processor to scan all inflight tasks
	// if domain needs to be failovered. However, in the multicursor queue logic, the scan range
	// can't be retrieved before the processor is started. If failover callback is registered
	// before queue processor is started, it may result in a deadline as to create the failover queue,
	// queue processor need to be started.
	e.registerDomainFailoverCallback()

	for _, replicationTaskProcessor := range e.replicationTaskProcessors {
		replicationTaskProcessor.Start()
	}
	if e.config.EnableGracefulFailover() {
		e.failoverMarkerNotifier.Start()
	}
}

// Stop the service.
func (e *historyEngineImpl) Stop() {
	e.logger.Info("History engine state changed", tag.LifeCycleStopping)
	defer e.logger.Info("History engine state changed", tag.LifeCycleStopped)

	e.txProcessor.Stop()
	e.timerProcessor.Stop()

	for _, replicationTaskProcessor := range e.replicationTaskProcessors {
		replicationTaskProcessor.Stop()
	}

	if e.queueTaskProcessor != nil {
		e.queueTaskProcessor.StopShardProcessor(e.shard)
	}

	e.failoverMarkerNotifier.Stop()

	// unset the failover callback
	e.shard.GetDomainCache().UnregisterDomainChangeCallback(e.shard.GetShardID())
}

func (e *historyEngineImpl) registerDomainFailoverCallback() {

	// NOTE: READ BEFORE MODIFICATION
	//
	// Tasks, e.g. transfer tasks and timer tasks, are created when holding the shard lock
	// meaning tasks -> release of shard lock
	//
	// Domain change notification follows the following steps, order matters
	// 1. lock all task processing.
	// 2. domain changes visible to everyone (Note: lock of task processing prevents task processing logic seeing the domain changes).
	// 3. failover min and max task levels are calculated, then update to shard.
	// 4. failover start & task processing unlock & shard domain version notification update. (order does not matter for this discussion)
	//
	// The above guarantees that task created during the failover will be processed.
	// If the task is created after domain change:
	// 		then active processor will handle it. (simple case)
	// If the task is created before domain change:
	//		task -> release of shard lock
	//		failover min / max task levels calculated & updated to shard (using shard lock) -> failover start
	// above 2 guarantees that failover start is after persistence of the task.

	failoverPredicate := func(shardNotificationVersion int64, nextDomain *cache.DomainCacheEntry, action func()) {
		domainFailoverNotificationVersion := nextDomain.GetFailoverNotificationVersion()
		domainActiveCluster := nextDomain.GetReplicationConfig().ActiveClusterName

		if nextDomain.IsGlobalDomain() &&
			domainFailoverNotificationVersion >= shardNotificationVersion &&
			domainActiveCluster == e.currentClusterName {
			action()
		}
	}

	// first set the failover callback
	e.shard.GetDomainCache().RegisterDomainChangeCallback(
		e.shard.GetShardID(),
		e.shard.GetDomainNotificationVersion(),
		func() {
			e.txProcessor.LockTaskProcessing()
			e.timerProcessor.LockTaskProcessing()
		},
		func(prevDomains []*cache.DomainCacheEntry, nextDomains []*cache.DomainCacheEntry) {
			defer func() {
				e.txProcessor.UnlockTaskProcessing()
				e.timerProcessor.UnlockTaskProcessing()
			}()

			if len(nextDomains) == 0 {
				return
			}

			shardNotificationVersion := e.shard.GetDomainNotificationVersion()
			failoverDomainIDs := map[string]struct{}{}

			for _, nextDomain := range nextDomains {
				failoverPredicate(shardNotificationVersion, nextDomain, func() {
					failoverDomainIDs[nextDomain.GetInfo().ID] = struct{}{}
				})
			}

			if len(failoverDomainIDs) > 0 {
				e.logger.Info("Domain Failover Start.", tag.WorkflowDomainIDs(failoverDomainIDs))

				e.txProcessor.FailoverDomain(failoverDomainIDs)
				e.timerProcessor.FailoverDomain(failoverDomainIDs)

				now := e.shard.GetTimeSource().Now()
				// the fake tasks will not be actually used, we just need to make sure
				// its length > 0 and has correct timestamp, to trigger a db scan
				fakeDecisionTask := []persistence.Task{&persistence.DecisionTask{}}
				fakeDecisionTimeoutTask := []persistence.Task{&persistence.DecisionTimeoutTask{VisibilityTimestamp: now}}
				e.txProcessor.NotifyNewTask(e.currentClusterName, nil, fakeDecisionTask)
				e.timerProcessor.NotifyNewTask(e.currentClusterName, nil, fakeDecisionTimeoutTask)
			}

			// handle graceful failover on active to passive
			// make sure task processor failover the domain before inserting the failover marker
			failoverMarkerTasks := []*persistence.FailoverMarkerTask{}
			for _, nextDomain := range nextDomains {
				domainFailoverNotificationVersion := nextDomain.GetFailoverNotificationVersion()
				domainActiveCluster := nextDomain.GetReplicationConfig().ActiveClusterName
				previousFailoverVersion := nextDomain.GetPreviousFailoverVersion()

				if nextDomain.IsGlobalDomain() &&
					domainFailoverNotificationVersion >= shardNotificationVersion &&
					domainActiveCluster != e.currentClusterName &&
					previousFailoverVersion != common.InitialPreviousFailoverVersion &&
					e.clusterMetadata.ClusterNameForFailoverVersion(previousFailoverVersion) == e.currentClusterName {
					// the visibility timestamp will be set in shard context
					failoverMarkerTasks = append(failoverMarkerTasks, &persistence.FailoverMarkerTask{
						Version:  nextDomain.GetFailoverVersion(),
						DomainID: nextDomain.GetInfo().ID,
					})
				}
			}

			if len(failoverMarkerTasks) > 0 {
				if err := e.shard.ReplicateFailoverMarkers(
					context.Background(),
					failoverMarkerTasks,
				); err != nil {
					e.logger.Error("Failed to insert failover marker to replication queue.", tag.Error(err))
					e.metricsClient.IncCounter(metrics.FailoverMarkerScope, metrics.FailoverMarkerInsertFailure)
					// fail this failover callback and it retries on next domain cache refresh
					return
				}
			}

			//nolint:errcheck
			e.shard.UpdateDomainNotificationVersion(nextDomains[len(nextDomains)-1].GetNotificationVersion() + 1)
		},
	)
}

func (e *historyEngineImpl) createMutableState(
	domainEntry *cache.DomainCacheEntry,
	runID string,
) (execution.MutableState, error) {

	newMutableState := execution.NewMutableStateBuilderWithVersionHistories(
		e.shard,
		e.logger,
		domainEntry,
	)

	if err := newMutableState.SetHistoryTree(runID); err != nil {
		return nil, err
	}

	return newMutableState, nil
}

func (e *historyEngineImpl) generateFirstDecisionTask(
	mutableState execution.MutableState,
	parentInfo *types.ParentExecutionInfo,
	startEvent *types.HistoryEvent,
) error {

	if parentInfo == nil {
		// DecisionTask is only created when it is not a Child Workflow and no backoff is needed
		if err := mutableState.AddFirstDecisionTaskScheduled(
			startEvent,
		); err != nil {
			return err
		}
	}
	return nil
}

// StartWorkflowExecution starts a workflow execution
func (e *historyEngineImpl) StartWorkflowExecution(
	ctx context.Context,
	startRequest *types.HistoryStartWorkflowExecutionRequest,
) (resp *types.StartWorkflowExecutionResponse, retError error) {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(startRequest.DomainUUID)
	if err != nil {
		return nil, err
	}

	return e.startWorkflowHelper(
		ctx,
		startRequest,
		domainEntry,
		metrics.HistoryStartWorkflowExecutionScope,
		nil)
}

// for startWorkflowHelper be reused by signalWithStart
type signalWithStartArg struct {
	signalWithStartRequest *types.HistorySignalWithStartWorkflowExecutionRequest
	prevMutableState       execution.MutableState
}

func (e *historyEngineImpl) newDomainNotActiveError(
	domainName string,
	failoverVersion int64,
) error {
	clusterMetadata := e.shard.GetService().GetClusterMetadata()
	return ce.NewDomainNotActiveError(
		domainName,
		clusterMetadata.GetCurrentClusterName(),
		clusterMetadata.ClusterNameForFailoverVersion(failoverVersion),
	)
}

func (e *historyEngineImpl) startWorkflowHelper(
	ctx context.Context,
	startRequest *types.HistoryStartWorkflowExecutionRequest,
	domainEntry *cache.DomainCacheEntry,
	metricsScope int,
	signalWithStartArg *signalWithStartArg,
) (resp *types.StartWorkflowExecutionResponse, retError error) {

	if domainEntry.GetInfo().Status != persistence.DomainStatusRegistered {
		return nil, errDomainDeprecated
	}

	request := startRequest.StartRequest
	err := validateStartWorkflowExecutionRequest(request, e.config.MaxIDLengthLimit())
	if err != nil {
		return nil, err
	}
	e.overrideStartWorkflowExecutionRequest(domainEntry, request, metricsScope)

	workflowID := request.GetWorkflowID()
	domainID := domainEntry.GetInfo().ID

	// grab the current context as a lock, nothing more
	// use a smaller context timeout to get the lock
	childCtx, childCancel := e.newChildContext(ctx)
	defer childCancel()

	_, currentRelease, err := e.executionCache.GetOrCreateCurrentWorkflowExecution(
		childCtx,
		domainID,
		workflowID,
	)
	if err != nil {
		if err == context.DeadlineExceeded {
			return nil, workflow.ErrConcurrentStartRequest
		}
		return nil, err
	}
	defer func() { currentRelease(retError) }()

	workflowExecution := types.WorkflowExecution{
		WorkflowID: workflowID,
		RunID:      uuid.New(),
	}
	curMutableState, err := e.createMutableState(domainEntry, workflowExecution.GetRunID())
	if err != nil {
		return nil, err
	}

	// preprocess for signalWithStart
	var prevMutableState execution.MutableState
	var signalWithStartRequest *types.HistorySignalWithStartWorkflowExecutionRequest
	isSignalWithStart := signalWithStartArg != nil
	if isSignalWithStart {
		prevMutableState = signalWithStartArg.prevMutableState
		signalWithStartRequest = signalWithStartArg.signalWithStartRequest
	}
	if prevMutableState != nil {
		prevLastWriteVersion, err := prevMutableState.GetLastWriteVersion()
		if err != nil {
			return nil, err
		}
		if prevLastWriteVersion > curMutableState.GetCurrentVersion() {
			return nil, e.newDomainNotActiveError(
				domainEntry.GetInfo().Name,
				prevLastWriteVersion,
			)
		}
		err = e.applyWorkflowIDReusePolicyForSigWithStart(
			prevMutableState.GetExecutionInfo(),
			workflowExecution,
			request.GetWorkflowIDReusePolicy(),
		)
		if err != nil {
			return nil, err
		}
	}

	err = e.addStartEventsAndTasks(
		curMutableState,
		workflowExecution,
		startRequest,
		signalWithStartRequest,
	)
	if err != nil {
		return nil, err
	}

	wfContext := execution.NewContext(domainID, workflowExecution, e.shard, e.executionManager, e.logger)

	now := e.timeSource.Now()
	newWorkflow, newWorkflowEventsSeq, err := curMutableState.CloseTransactionAsSnapshot(
		now,
		execution.TransactionPolicyActive,
	)
	if err != nil {
		return nil, err
	}
	historySize, err := wfContext.PersistFirstWorkflowEvents(ctx, newWorkflowEventsSeq[0])
	if err != nil {
		return nil, err
	}

	// create as brand new
	createMode := persistence.CreateWorkflowModeBrandNew
	prevRunID := ""
	prevLastWriteVersion := int64(0)
	// overwrite in case of signalWithStart
	if prevMutableState != nil {
		createMode = persistence.CreateWorkflowModeWorkflowIDReuse
		prevRunID = prevMutableState.GetExecutionInfo().RunID
		prevLastWriteVersion, err = prevMutableState.GetLastWriteVersion()
		if err != nil {
			return nil, err
		}
	}
	err = wfContext.CreateWorkflowExecution(
		ctx,
		newWorkflow,
		historySize,
		now,
		createMode,
		prevRunID,
		prevLastWriteVersion,
	)
	// handle already started error
	if t, ok := err.(*persistence.WorkflowExecutionAlreadyStartedError); ok {

		if t.StartRequestID == request.GetRequestID() {
			return &types.StartWorkflowExecutionResponse{
				RunID: t.RunID,
			}, nil
		}

		if isSignalWithStart {
			return nil, err
		}

		if curMutableState.GetCurrentVersion() < t.LastWriteVersion {
			return nil, e.newDomainNotActiveError(
				domainEntry.GetInfo().Name,
				t.LastWriteVersion,
			)
		}

		prevRunID = t.RunID
		if shouldTerminateAndStart(startRequest, t.State) {
			runningWFCtx, err := workflow.LoadOnce(ctx, e.executionCache, domainID, workflowID, prevRunID)
			if err != nil {
				return nil, err
			}
			defer func() { runningWFCtx.GetReleaseFn()(retError) }()

			return e.terminateAndStartWorkflow(
				ctx,
				runningWFCtx,
				workflowExecution,
				domainEntry,
				domainID,
				startRequest,
				nil,
			)
		}
		if err = e.applyWorkflowIDReusePolicyHelper(
			t.StartRequestID,
			prevRunID,
			t.State,
			t.CloseStatus,
			workflowExecution,
			startRequest.StartRequest.GetWorkflowIDReusePolicy(),
		); err != nil {
			return nil, err
		}
		// create as ID reuse
		createMode = persistence.CreateWorkflowModeWorkflowIDReuse
		err = wfContext.CreateWorkflowExecution(
			ctx,
			newWorkflow,
			historySize,
			now,
			createMode,
			prevRunID,
			t.LastWriteVersion,
		)
	}
	if err != nil {
		return nil, err
	}

	return &types.StartWorkflowExecutionResponse{
		RunID: workflowExecution.RunID,
	}, nil
}

func shouldTerminateAndStart(
	startRequest *types.HistoryStartWorkflowExecutionRequest,
	state int,
) bool {
	return startRequest.StartRequest.GetWorkflowIDReusePolicy() == types.WorkflowIDReusePolicyTerminateIfRunning &&
		(state == persistence.WorkflowStateRunning || state == persistence.WorkflowStateCreated)
}

// terminate running workflow then start a new run in one transaction
func (e *historyEngineImpl) terminateAndStartWorkflow(
	ctx context.Context,
	runningWFCtx workflow.Context,
	workflowExecution types.WorkflowExecution,
	domainEntry *cache.DomainCacheEntry,
	domainID string,
	startRequest *types.HistoryStartWorkflowExecutionRequest,
	signalWithStartRequest *types.HistorySignalWithStartWorkflowExecutionRequest,
) (*types.StartWorkflowExecutionResponse, error) {
	runningMutableState := runningWFCtx.GetMutableState()
UpdateWorkflowLoop:
	for attempt := 0; attempt < workflow.ConditionalRetryCount; attempt++ {
		if !runningMutableState.IsWorkflowExecutionRunning() {
			return nil, workflow.ErrAlreadyCompleted
		}

		if err := execution.TerminateWorkflow(
			runningMutableState,
			runningMutableState.GetNextEventID(),
			TerminateIfRunningReason,
			getTerminateIfRunningDetails(workflowExecution.GetRunID()),
			execution.IdentityHistoryService,
		); err != nil {
			if err == workflow.ErrStaleState {
				// Handler detected that cached workflow mutable could potentially be stale
				// Reload workflow execution history
				runningWFCtx.GetContext().Clear()
				if attempt != workflow.ConditionalRetryCount-1 {
					_, err = runningWFCtx.ReloadMutableState(ctx)
					if err != nil {
						return nil, err
					}
				}
				continue UpdateWorkflowLoop
			}
			return nil, err
		}

		// new mutable state
		newMutableState, err := e.createMutableState(domainEntry, workflowExecution.GetRunID())
		if err != nil {
			return nil, err
		}

		if signalWithStartRequest != nil {
			startRequest, err = getStartRequest(domainID, signalWithStartRequest.SignalWithStartRequest)
			if err != nil {
				return nil, err
			}
		}

		err = e.addStartEventsAndTasks(
			newMutableState,
			workflowExecution,
			startRequest,
			signalWithStartRequest,
		)
		if err != nil {
			return nil, err
		}

		updateErr := runningWFCtx.GetContext().UpdateWorkflowExecutionWithNewAsActive(
			ctx,
			e.timeSource.Now(),
			execution.NewContext(
				domainID,
				workflowExecution,
				e.shard,
				e.shard.GetExecutionManager(),
				e.logger,
			),
			newMutableState,
		)
		if updateErr != nil {
			if updateErr == execution.ErrConflict {
				e.metricsClient.IncCounter(metrics.HistoryStartWorkflowExecutionScope, metrics.ConcurrencyUpdateFailureCounter)
				continue UpdateWorkflowLoop
			}
			return nil, updateErr
		}
		break UpdateWorkflowLoop
	}
	return &types.StartWorkflowExecutionResponse{
		RunID: workflowExecution.RunID,
	}, nil
}

func (e *historyEngineImpl) addStartEventsAndTasks(
	mutableState execution.MutableState,
	workflowExecution types.WorkflowExecution,
	startRequest *types.HistoryStartWorkflowExecutionRequest,
	signalWithStartRequest *types.HistorySignalWithStartWorkflowExecutionRequest,
) error {
	// Add WF start event
	startEvent, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		startRequest,
	)
	if err != nil {
		return &types.InternalServiceError{
			Message: "Failed to add workflow execution started event.",
		}
	}

	if signalWithStartRequest != nil {
		// Add signal event
		sRequest := signalWithStartRequest.SignalWithStartRequest
		_, err := mutableState.AddWorkflowExecutionSignaled(
			sRequest.GetSignalName(),
			sRequest.GetSignalInput(),
			sRequest.GetIdentity())
		if err != nil {
			return &types.InternalServiceError{Message: "Failed to add workflow execution signaled event."}
		}
	}

	// Generate first decision task event if not child WF and no first decision task backoff
	return e.generateFirstDecisionTask(
		mutableState,
		startRequest.ParentExecutionInfo,
		startEvent,
	)
}

func getTerminateIfRunningDetails(newRunID string) []byte {
	return []byte(fmt.Sprintf(TerminateIfRunningDetailsTemplate, newRunID))
}

// GetMutableState retrieves the mutable state of the workflow execution
func (e *historyEngineImpl) GetMutableState(
	ctx context.Context,
	request *types.GetMutableStateRequest,
) (*types.GetMutableStateResponse, error) {

	return e.getMutableStateOrPolling(ctx, request)
}

// PollMutableState retrieves the mutable state of the workflow execution with long polling
func (e *historyEngineImpl) PollMutableState(
	ctx context.Context,
	request *types.PollMutableStateRequest,
) (*types.PollMutableStateResponse, error) {

	response, err := e.getMutableStateOrPolling(ctx, &types.GetMutableStateRequest{
		DomainUUID:          request.DomainUUID,
		Execution:           request.Execution,
		ExpectedNextEventID: request.ExpectedNextEventID,
		CurrentBranchToken:  request.CurrentBranchToken})

	if err != nil {
		return nil, e.updateEntityNotExistsErrorOnPassiveCluster(err, request.GetDomainUUID())
	}

	return &types.PollMutableStateResponse{
		Execution:                            response.Execution,
		WorkflowType:                         response.WorkflowType,
		NextEventID:                          response.NextEventID,
		PreviousStartedEventID:               response.PreviousStartedEventID,
		LastFirstEventID:                     response.LastFirstEventID,
		TaskList:                             response.TaskList,
		StickyTaskList:                       response.StickyTaskList,
		ClientLibraryVersion:                 response.ClientLibraryVersion,
		ClientFeatureVersion:                 response.ClientFeatureVersion,
		ClientImpl:                           response.ClientImpl,
		StickyTaskListScheduleToStartTimeout: response.StickyTaskListScheduleToStartTimeout,
		CurrentBranchToken:                   response.CurrentBranchToken,
		VersionHistories:                     response.VersionHistories,
		WorkflowState:                        response.WorkflowState,
		WorkflowCloseState:                   response.WorkflowCloseState,
	}, nil
}

func (e *historyEngineImpl) updateEntityNotExistsErrorOnPassiveCluster(err error, domainID string) error {
	switch err.(type) {
	case *types.EntityNotExistsError:
		domainCache, domainCacheErr := e.shard.GetDomainCache().GetDomainByID(domainID)
		if domainCacheErr != nil {
			return err // if could not access domain cache simply return original error
		}

		if domainNotActiveErr := domainCache.GetDomainNotActiveErr(); domainNotActiveErr != nil {
			domainNotActiveErrCasted := domainNotActiveErr.(*types.DomainNotActiveError)
			return &types.EntityNotExistsError{
				Message:        "Workflow execution not found in non-active cluster",
				ActiveCluster:  domainNotActiveErrCasted.GetActiveCluster(),
				CurrentCluster: domainNotActiveErrCasted.GetCurrentCluster(),
			}
		}
	}
	return err
}

func (e *historyEngineImpl) getMutableStateOrPolling(
	ctx context.Context,
	request *types.GetMutableStateRequest,
) (*types.GetMutableStateResponse, error) {

	if err := common.ValidateDomainUUID(request.DomainUUID); err != nil {
		return nil, err
	}
	domainID := request.DomainUUID
	execution := types.WorkflowExecution{
		WorkflowID: request.Execution.WorkflowID,
		RunID:      request.Execution.RunID,
	}
	response, err := e.getMutableState(ctx, domainID, execution)
	if err != nil {
		return nil, err
	}
	if request.CurrentBranchToken == nil {
		request.CurrentBranchToken = response.CurrentBranchToken
	}
	if !bytes.Equal(request.CurrentBranchToken, response.CurrentBranchToken) {
		return nil, &types.CurrentBranchChangedError{
			Message:            "current branch token and request branch token doesn't match",
			CurrentBranchToken: response.CurrentBranchToken}
	}
	// set the run id in case query the current running workflow
	execution.RunID = response.Execution.RunID

	// expectedNextEventID is 0 when caller want to get the current next event ID without blocking
	expectedNextEventID := common.FirstEventID
	if request.ExpectedNextEventID != 0 {
		expectedNextEventID = request.GetExpectedNextEventID()
	}

	// if caller decide to long poll on workflow execution
	// and the event ID we are looking for is smaller than current next event ID
	if expectedNextEventID >= response.GetNextEventID() && response.GetIsWorkflowRunning() {
		subscriberID, channel, err := e.historyEventNotifier.WatchHistoryEvent(definition.NewWorkflowIdentifier(domainID, execution.GetWorkflowID(), execution.GetRunID()))
		if err != nil {
			return nil, err
		}
		defer e.historyEventNotifier.UnwatchHistoryEvent(definition.NewWorkflowIdentifier(domainID, execution.GetWorkflowID(), execution.GetRunID()), subscriberID) //nolint:errcheck
		// check again in case the next event ID is updated
		response, err = e.getMutableState(ctx, domainID, execution)
		if err != nil {
			return nil, err
		}
		// check again if the current branch token changed
		if !bytes.Equal(request.CurrentBranchToken, response.CurrentBranchToken) {
			return nil, &types.CurrentBranchChangedError{
				Message:            "current branch token and request branch token doesn't match",
				CurrentBranchToken: response.CurrentBranchToken}
		}
		if expectedNextEventID < response.GetNextEventID() || !response.GetIsWorkflowRunning() {
			return response, nil
		}

		domainName, err := e.shard.GetDomainCache().GetDomainName(domainID)
		if err != nil {
			return nil, err
		}
		timer := time.NewTimer(e.shard.GetConfig().LongPollExpirationInterval(domainName))
		defer timer.Stop()
		for {
			select {
			case event := <-channel:
				response.LastFirstEventID = event.LastFirstEventID
				response.NextEventID = event.NextEventID
				response.IsWorkflowRunning = event.WorkflowCloseState == persistence.WorkflowCloseStatusNone
				response.PreviousStartedEventID = common.Int64Ptr(event.PreviousStartedEventID)
				response.WorkflowState = common.Int32Ptr(int32(event.WorkflowState))
				response.WorkflowCloseState = common.Int32Ptr(int32(event.WorkflowCloseState))
				if !bytes.Equal(request.CurrentBranchToken, event.CurrentBranchToken) {
					return nil, &types.CurrentBranchChangedError{
						Message:            "Current branch token and request branch token doesn't match",
						CurrentBranchToken: event.CurrentBranchToken}
				}
				if expectedNextEventID < response.GetNextEventID() || !response.GetIsWorkflowRunning() {
					return response, nil
				}
			case <-timer.C:
				return response, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	return response, nil
}

func (e *historyEngineImpl) QueryWorkflow(
	ctx context.Context,
	request *types.HistoryQueryWorkflowRequest,
) (retResp *types.HistoryQueryWorkflowResponse, retErr error) {

	scope := e.metricsClient.Scope(metrics.HistoryQueryWorkflowScope).Tagged(metrics.DomainTag(request.GetRequest().GetDomain()))

	consistentQueryEnabled := e.config.EnableConsistentQuery() && e.config.EnableConsistentQueryByDomain(request.GetRequest().GetDomain())
	if request.GetRequest().GetQueryConsistencyLevel() == types.QueryConsistencyLevelStrong && !consistentQueryEnabled {
		return nil, workflow.ErrConsistentQueryNotEnabled
	}

	execution := *request.GetRequest().GetExecution()

	mutableStateResp, err := e.getMutableState(ctx, request.GetDomainUUID(), execution)
	if err != nil {
		return nil, err
	}
	req := request.GetRequest()
	if !mutableStateResp.GetIsWorkflowRunning() && req.QueryRejectCondition != nil {
		notOpenReject := req.GetQueryRejectCondition() == types.QueryRejectConditionNotOpen
		closeStatus := mutableStateResp.GetWorkflowCloseState()
		notCompletedCleanlyReject := req.GetQueryRejectCondition() == types.QueryRejectConditionNotCompletedCleanly && closeStatus != persistence.WorkflowCloseStatusCompleted
		if notOpenReject || notCompletedCleanlyReject {
			return &types.HistoryQueryWorkflowResponse{
				Response: &types.QueryWorkflowResponse{
					QueryRejected: &types.QueryRejected{
						CloseStatus: persistence.ToInternalWorkflowExecutionCloseStatus(int(closeStatus)).Ptr(),
					},
				},
			}, nil
		}
	}

	// query cannot be processed unless at least one decision task has finished
	// if first decision task has not finished wait for up to a second for it to complete
	queryFirstDecisionTaskWaitTime := defaultQueryFirstDecisionTaskWaitTime
	ctxDeadline, ok := ctx.Deadline()
	if ok {
		ctxWaitTime := ctxDeadline.Sub(time.Now()) - time.Second
		if ctxWaitTime > queryFirstDecisionTaskWaitTime {
			queryFirstDecisionTaskWaitTime = ctxWaitTime
		}
	}
	deadline := time.Now().Add(queryFirstDecisionTaskWaitTime)
	for mutableStateResp.GetPreviousStartedEventID() <= 0 && time.Now().Before(deadline) {
		<-time.After(queryFirstDecisionTaskCheckInterval)
		mutableStateResp, err = e.getMutableState(ctx, request.GetDomainUUID(), execution)
		if err != nil {
			return nil, err
		}
	}

	if mutableStateResp.GetPreviousStartedEventID() <= 0 {
		scope.IncCounter(metrics.QueryBeforeFirstDecisionCount)
		return nil, workflow.ErrQueryWorkflowBeforeFirstDecision
	}

	de, err := e.shard.GetDomainCache().GetDomainByID(request.GetDomainUUID())
	if err != nil {
		return nil, err
	}

	wfContext, release, err := e.executionCache.GetOrCreateWorkflowExecution(ctx, request.GetDomainUUID(), execution)
	if err != nil {
		return nil, err
	}
	defer func() { release(retErr) }()
	mutableState, err := wfContext.LoadWorkflowExecution(ctx)
	if err != nil {
		return nil, err
	}

	// There are two ways in which queries get dispatched to decider. First, queries can be dispatched on decision tasks.
	// These decision tasks potentially contain new events and queries. The events are treated as coming before the query in time.
	// The second way in which queries are dispatched to decider is directly through matching; in this approach queries can be
	// dispatched to decider immediately even if there are outstanding events that came before the query. The following logic
	// is used to determine if a query can be safely dispatched directly through matching or if given the desired consistency
	// level must be dispatched on a decision task. There are four cases in which a query can be dispatched directly through
	// matching safely, without violating the desired consistency level:
	// 1. the domain is not active, in this case history is immutable so a query dispatched at any time is consistent
	// 2. the workflow is not running, whenever a workflow is not running dispatching query directly is consistent
	// 3. the client requested eventual consistency, in this case there are no consistency requirements so dispatching directly through matching is safe
	// 4. if there is no pending or started decision it means no events came before query arrived, so its safe to dispatch directly
	safeToDispatchDirectly := !de.IsDomainActive() ||
		!mutableState.IsWorkflowExecutionRunning() ||
		req.GetQueryConsistencyLevel() == types.QueryConsistencyLevelEventual ||
		(!mutableState.HasPendingDecision() && !mutableState.HasInFlightDecision())
	if safeToDispatchDirectly {
		release(nil)
		msResp, err := e.getMutableState(ctx, request.GetDomainUUID(), execution)
		if err != nil {
			return nil, err
		}
		req.Execution.RunID = msResp.Execution.RunID
		return e.queryDirectlyThroughMatching(ctx, msResp, request.GetDomainUUID(), req, scope)
	}

	// If we get here it means query could not be dispatched through matching directly, so it must block
	// until either an result has been obtained on a decision task response or until it is safe to dispatch directly through matching.
	sw := scope.StartTimer(metrics.DecisionTaskQueryLatency)
	defer sw.Stop()
	queryReg := mutableState.GetQueryRegistry()
	if len(queryReg.GetBufferedIDs()) >= e.config.MaxBufferedQueryCount() {
		scope.IncCounter(metrics.QueryBufferExceededCount)
		return nil, workflow.ErrConsistentQueryBufferExceeded
	}
	queryID, termCh := queryReg.BufferQuery(req.GetQuery())
	defer queryReg.RemoveQuery(queryID)
	release(nil)
	select {
	case <-termCh:
		state, err := queryReg.GetTerminationState(queryID)
		if err != nil {
			scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			return nil, err
		}
		switch state.TerminationType {
		case query.TerminationTypeCompleted:
			result := state.QueryResult
			switch result.GetResultType() {
			case types.QueryResultTypeAnswered:
				return &types.HistoryQueryWorkflowResponse{
					Response: &types.QueryWorkflowResponse{
						QueryResult: result.GetAnswer(),
					},
				}, nil
			case types.QueryResultTypeFailed:
				return nil, &types.QueryFailedError{Message: result.GetErrorMessage()}
			default:
				scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
				return nil, workflow.ErrQueryEnteredInvalidState
			}
		case query.TerminationTypeUnblocked:
			msResp, err := e.getMutableState(ctx, request.GetDomainUUID(), execution)
			if err != nil {
				return nil, err
			}
			req.Execution.RunID = msResp.Execution.RunID
			return e.queryDirectlyThroughMatching(ctx, msResp, request.GetDomainUUID(), req, scope)
		case query.TerminationTypeFailed:
			return nil, state.Failure
		default:
			scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			return nil, workflow.ErrQueryEnteredInvalidState
		}
	case <-ctx.Done():
		scope.IncCounter(metrics.ConsistentQueryTimeoutCount)
		return nil, ctx.Err()
	}
}

func (e *historyEngineImpl) queryDirectlyThroughMatching(
	ctx context.Context,
	msResp *types.GetMutableStateResponse,
	domainID string,
	queryRequest *types.QueryWorkflowRequest,
	scope metrics.Scope,
) (*types.HistoryQueryWorkflowResponse, error) {

	sw := scope.StartTimer(metrics.DirectQueryDispatchLatency)
	defer sw.Stop()

	// Sticky task list is not very useful in the standby cluster because the decider cache is
	// not updated by dispatching tasks to it (it is only updated in the case of query).
	// Additionally on the standby side we are not even able to clear sticky.
	// Stickiness might be outdated if the customer did a restart of their nodes causing a query
	// dispatched on the standby side on sticky to hang. We decided it made sense to simply not attempt
	// query on sticky task list at all on the passive side.
	de, err := e.shard.GetDomainCache().GetDomainByID(domainID)
	if err != nil {
		return nil, err
	}
	supportsStickyQuery := e.clientChecker.SupportsStickyQuery(msResp.GetClientImpl(), msResp.GetClientFeatureVersion()) == nil
	if msResp.GetIsStickyTaskListEnabled() &&
		len(msResp.GetStickyTaskList().GetName()) != 0 &&
		supportsStickyQuery &&
		e.config.EnableStickyQuery(queryRequest.GetDomain()) &&
		de.IsDomainActive() {

		stickyMatchingRequest := &types.MatchingQueryWorkflowRequest{
			DomainUUID:   domainID,
			QueryRequest: queryRequest,
			TaskList:     msResp.GetStickyTaskList(),
		}

		// using a clean new context in case customer provide a context which has
		// a really short deadline, causing we clear the stickiness
		stickyContext, cancel := context.WithTimeout(context.Background(), time.Duration(msResp.GetStickyTaskListScheduleToStartTimeout())*time.Second)
		stickyStopWatch := scope.StartTimer(metrics.DirectQueryDispatchStickyLatency)
		matchingResp, err := e.rawMatchingClient.QueryWorkflow(stickyContext, stickyMatchingRequest)
		stickyStopWatch.Stop()
		cancel()
		if err == nil {
			scope.IncCounter(metrics.DirectQueryDispatchStickySuccessCount)
			return &types.HistoryQueryWorkflowResponse{Response: matchingResp}, nil
		}
		if yarpcError, ok := err.(*yarpcerrors.Status); !ok || yarpcError.Code() != yarpcerrors.CodeDeadlineExceeded {
			e.logger.Error("query directly though matching on sticky failed, will not attempt query on non-sticky",
				tag.WorkflowDomainName(queryRequest.GetDomain()),
				tag.WorkflowID(queryRequest.Execution.GetWorkflowID()),
				tag.WorkflowRunID(queryRequest.Execution.GetRunID()),
				tag.WorkflowQueryType(queryRequest.Query.GetQueryType()),
				tag.Error(err))
			return nil, err
		}
		if msResp.GetIsWorkflowRunning() {
			e.logger.Info("query direct through matching failed on sticky, clearing sticky before attempting on non-sticky",
				tag.WorkflowDomainName(queryRequest.GetDomain()),
				tag.WorkflowID(queryRequest.Execution.GetWorkflowID()),
				tag.WorkflowRunID(queryRequest.Execution.GetRunID()),
				tag.WorkflowQueryType(queryRequest.Query.GetQueryType()))
			resetContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			clearStickinessStopWatch := scope.StartTimer(metrics.DirectQueryDispatchClearStickinessLatency)
			_, err := e.ResetStickyTaskList(resetContext, &types.HistoryResetStickyTaskListRequest{
				DomainUUID: domainID,
				Execution:  queryRequest.GetExecution(),
			})
			clearStickinessStopWatch.Stop()
			cancel()
			if err != nil && err != workflow.ErrAlreadyCompleted {
				return nil, err
			}
			scope.IncCounter(metrics.DirectQueryDispatchClearStickinessSuccessCount)
		}
	}

	if err := common.IsValidContext(ctx); err != nil {
		e.logger.Info("query context timed out before query on non-sticky task list could be attempted",
			tag.WorkflowDomainName(queryRequest.GetDomain()),
			tag.WorkflowID(queryRequest.Execution.GetWorkflowID()),
			tag.WorkflowRunID(queryRequest.Execution.GetRunID()),
			tag.WorkflowQueryType(queryRequest.Query.GetQueryType()))
		scope.IncCounter(metrics.DirectQueryDispatchTimeoutBeforeNonStickyCount)
		return nil, err
	}

	e.logger.Debug("query directly through matching on sticky timed out, attempting to query on non-sticky",
		tag.WorkflowDomainName(queryRequest.GetDomain()),
		tag.WorkflowID(queryRequest.Execution.GetWorkflowID()),
		tag.WorkflowRunID(queryRequest.Execution.GetRunID()),
		tag.WorkflowQueryType(queryRequest.Query.GetQueryType()),
		tag.WorkflowTaskListName(msResp.GetStickyTaskList().GetName()),
		tag.WorkflowNextEventID(msResp.GetNextEventID()))

	nonStickyMatchingRequest := &types.MatchingQueryWorkflowRequest{
		DomainUUID:   domainID,
		QueryRequest: queryRequest,
		TaskList:     msResp.TaskList,
	}

	nonStickyStopWatch := scope.StartTimer(metrics.DirectQueryDispatchNonStickyLatency)
	matchingResp, err := e.matchingClient.QueryWorkflow(ctx, nonStickyMatchingRequest)
	nonStickyStopWatch.Stop()
	if err != nil {
		e.logger.Error("query directly though matching on non-sticky failed",
			tag.WorkflowDomainName(queryRequest.GetDomain()),
			tag.WorkflowID(queryRequest.Execution.GetWorkflowID()),
			tag.WorkflowRunID(queryRequest.Execution.GetRunID()),
			tag.WorkflowQueryType(queryRequest.Query.GetQueryType()),
			tag.Error(err))
		return nil, err
	}
	scope.IncCounter(metrics.DirectQueryDispatchNonStickySuccessCount)
	return &types.HistoryQueryWorkflowResponse{Response: matchingResp}, err
}

func (e *historyEngineImpl) getMutableState(
	ctx context.Context,
	domainID string,
	execution types.WorkflowExecution,
) (retResp *types.GetMutableStateResponse, retError error) {

	wfContext, release, retError := e.executionCache.GetOrCreateWorkflowExecution(ctx, domainID, execution)
	if retError != nil {
		return
	}
	defer func() { release(retError) }()

	mutableState, retError := wfContext.LoadWorkflowExecution(ctx)
	if retError != nil {
		return
	}

	currentBranchToken, err := mutableState.GetCurrentBranchToken()
	if err != nil {
		return nil, err
	}

	executionInfo := mutableState.GetExecutionInfo()
	execution.RunID = wfContext.GetExecution().RunID
	workflowState, workflowCloseState := mutableState.GetWorkflowStateCloseStatus()
	retResp = &types.GetMutableStateResponse{
		Execution:                            &execution,
		WorkflowType:                         &types.WorkflowType{Name: executionInfo.WorkflowTypeName},
		LastFirstEventID:                     mutableState.GetLastFirstEventID(),
		NextEventID:                          mutableState.GetNextEventID(),
		PreviousStartedEventID:               common.Int64Ptr(mutableState.GetPreviousStartedEventID()),
		TaskList:                             &types.TaskList{Name: executionInfo.TaskList},
		StickyTaskList:                       &types.TaskList{Name: executionInfo.StickyTaskList},
		ClientLibraryVersion:                 executionInfo.ClientLibraryVersion,
		ClientFeatureVersion:                 executionInfo.ClientFeatureVersion,
		ClientImpl:                           executionInfo.ClientImpl,
		IsWorkflowRunning:                    mutableState.IsWorkflowExecutionRunning(),
		StickyTaskListScheduleToStartTimeout: common.Int32Ptr(executionInfo.StickyScheduleToStartTimeout),
		CurrentBranchToken:                   currentBranchToken,
		WorkflowState:                        common.Int32Ptr(int32(workflowState)),
		WorkflowCloseState:                   common.Int32Ptr(int32(workflowCloseState)),
		IsStickyTaskListEnabled:              mutableState.IsStickyTaskListEnabled(),
	}
	versionHistories := mutableState.GetVersionHistories()
	if versionHistories != nil {
		retResp.VersionHistories = versionHistories.ToInternalType()
	}
	return
}

func (e *historyEngineImpl) DescribeMutableState(
	ctx context.Context,
	request *types.DescribeMutableStateRequest,
) (response *types.DescribeMutableStateResponse, retError error) {

	if err := common.ValidateDomainUUID(request.DomainUUID); err != nil {
		return nil, err
	}

	domainID := request.DomainUUID
	execution := types.WorkflowExecution{
		WorkflowID: request.Execution.WorkflowID,
		RunID:      request.Execution.RunID,
	}

	cacheCtx, dbCtx, release, cacheHit, err := e.executionCache.GetAndCreateWorkflowExecution(
		ctx, domainID, execution,
	)
	if err != nil {
		return nil, err
	}
	defer func() { release(retError) }()

	response = &types.DescribeMutableStateResponse{}

	if cacheHit {
		if msb := cacheCtx.GetWorkflowExecution(); msb != nil {
			response.MutableStateInCache, err = e.toMutableStateJSON(msb)
			if err != nil {
				return nil, err
			}
		}
	}

	msb, err := dbCtx.LoadWorkflowExecution(ctx)
	if err != nil {
		return nil, err
	}
	response.MutableStateInDatabase, err = e.toMutableStateJSON(msb)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (e *historyEngineImpl) toMutableStateJSON(msb execution.MutableState) (string, error) {
	ms := msb.CopyToPersistence()

	jsonBytes, err := json.Marshal(ms)
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}

// ResetStickyTaskList reset the volatile information in mutable state of a given types.
// Volatile information are the information related to client, such as:
// 1. StickyTaskList
// 2. StickyScheduleToStartTimeout
// 3. ClientLibraryVersion
// 4. ClientFeatureVersion
// 5. ClientImpl
func (e *historyEngineImpl) ResetStickyTaskList(
	ctx context.Context,
	resetRequest *types.HistoryResetStickyTaskListRequest,
) (*types.HistoryResetStickyTaskListResponse, error) {

	if err := common.ValidateDomainUUID(resetRequest.DomainUUID); err != nil {
		return nil, err
	}
	domainID := resetRequest.DomainUUID

	err := workflow.UpdateWithAction(ctx, e.executionCache, domainID, *resetRequest.Execution, false, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				return workflow.ErrAlreadyCompleted
			}
			mutableState.ClearStickyness()
			return nil
		},
	)

	if err != nil {
		return nil, err
	}
	return &types.HistoryResetStickyTaskListResponse{}, nil
}

// DescribeWorkflowExecution returns information about the specified workflow execution.
func (e *historyEngineImpl) DescribeWorkflowExecution(
	ctx context.Context,
	request *types.HistoryDescribeWorkflowExecutionRequest,
) (retResp *types.DescribeWorkflowExecutionResponse, retError error) {

	if err := common.ValidateDomainUUID(request.DomainUUID); err != nil {
		return nil, err
	}

	domainID := request.DomainUUID
	execution := *request.Request.Execution

	wfContext, release, err0 := e.executionCache.GetOrCreateWorkflowExecution(ctx, domainID, execution)
	if err0 != nil {
		return nil, err0
	}
	defer func() { release(retError) }()

	mutableState, err1 := wfContext.LoadWorkflowExecution(ctx)
	if err1 != nil {
		return nil, err1
	}
	executionInfo := mutableState.GetExecutionInfo()

	result := &types.DescribeWorkflowExecutionResponse{
		ExecutionConfiguration: &types.WorkflowExecutionConfiguration{
			TaskList:                            &types.TaskList{Name: executionInfo.TaskList},
			ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(executionInfo.WorkflowTimeout),
			TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(executionInfo.DecisionStartToCloseTimeout),
		},
		WorkflowExecutionInfo: &types.WorkflowExecutionInfo{
			Execution: &types.WorkflowExecution{
				WorkflowID: executionInfo.WorkflowID,
				RunID:      executionInfo.RunID,
			},
			Type:             &types.WorkflowType{Name: executionInfo.WorkflowTypeName},
			StartTime:        common.Int64Ptr(executionInfo.StartTimestamp.UnixNano()),
			HistoryLength:    mutableState.GetNextEventID() - common.FirstEventID,
			AutoResetPoints:  executionInfo.AutoResetPoints,
			Memo:             &types.Memo{Fields: executionInfo.Memo},
			SearchAttributes: &types.SearchAttributes{IndexedFields: executionInfo.SearchAttributes},
		},
	}

	// TODO: we need to consider adding execution time to mutable state
	// For now execution time will be calculated based on start time and cron schedule/retry policy
	// each time DescribeWorkflowExecution is called.
	startEvent, err := mutableState.GetStartEvent(ctx)
	if err != nil {
		return nil, err
	}
	backoffDuration := time.Duration(startEvent.GetWorkflowExecutionStartedEventAttributes().GetFirstDecisionTaskBackoffSeconds()) * time.Second
	result.WorkflowExecutionInfo.ExecutionTime = common.Int64Ptr(result.WorkflowExecutionInfo.GetStartTime() + backoffDuration.Nanoseconds())

	if executionInfo.ParentRunID != "" {
		result.WorkflowExecutionInfo.ParentExecution = &types.WorkflowExecution{
			WorkflowID: executionInfo.ParentWorkflowID,
			RunID:      executionInfo.ParentRunID,
		}
		result.WorkflowExecutionInfo.ParentDomainID = common.StringPtr(executionInfo.ParentDomainID)
		result.WorkflowExecutionInfo.ParentInitiatedID = common.Int64Ptr(executionInfo.InitiatedID)
		if entry, err := e.shard.GetDomainCache().GetActiveDomainByID(executionInfo.ParentDomainID); err == nil {
			result.WorkflowExecutionInfo.ParentDomain = common.StringPtr(entry.GetInfo().Name)
		}
	}
	if executionInfo.State == persistence.WorkflowStateCompleted {
		// for closed workflow
		closeStatus := persistence.ToInternalWorkflowExecutionCloseStatus(executionInfo.CloseStatus)
		result.WorkflowExecutionInfo.CloseStatus = &closeStatus
		completionEvent, err := mutableState.GetCompletionEvent(ctx)
		if err != nil {
			return nil, err
		}
		result.WorkflowExecutionInfo.CloseTime = common.Int64Ptr(completionEvent.GetTimestamp())
	}

	if len(mutableState.GetPendingActivityInfos()) > 0 {
		for _, ai := range mutableState.GetPendingActivityInfos() {
			p := &types.PendingActivityInfo{
				ActivityID: ai.ActivityID,
			}
			state := types.PendingActivityStateScheduled
			if ai.CancelRequested {
				state = types.PendingActivityStateCancelRequested
			} else if ai.StartedID != common.EmptyEventID {
				state = types.PendingActivityStateStarted
			}
			p.State = &state
			lastHeartbeatUnixNano := ai.LastHeartBeatUpdatedTime.UnixNano()
			if lastHeartbeatUnixNano > 0 {
				p.LastHeartbeatTimestamp = common.Int64Ptr(lastHeartbeatUnixNano)
				p.HeartbeatDetails = ai.Details
			}
			// TODO: move to mutable state instead of loading it from event
			scheduledEvent, err := mutableState.GetActivityScheduledEvent(ctx, ai.ScheduleID)
			if err != nil {
				return nil, err
			}
			p.ActivityType = scheduledEvent.ActivityTaskScheduledEventAttributes.ActivityType
			if state == types.PendingActivityStateScheduled {
				p.ScheduledTimestamp = common.Int64Ptr(ai.ScheduledTime.UnixNano())
			} else {
				p.LastStartedTimestamp = common.Int64Ptr(ai.StartedTime.UnixNano())
			}
			if ai.HasRetryPolicy {
				p.Attempt = ai.Attempt
				p.ExpirationTimestamp = common.Int64Ptr(ai.ExpirationTime.UnixNano())
				if ai.MaximumAttempts != 0 {
					p.MaximumAttempts = ai.MaximumAttempts
				}
				if ai.LastFailureReason != "" {
					p.LastFailureReason = common.StringPtr(ai.LastFailureReason)
					p.LastFailureDetails = ai.LastFailureDetails
				}
				if ai.LastWorkerIdentity != "" {
					p.LastWorkerIdentity = ai.LastWorkerIdentity
				}
			}
			result.PendingActivities = append(result.PendingActivities, p)
		}
	}

	if len(mutableState.GetPendingChildExecutionInfos()) > 0 {
		for _, ch := range mutableState.GetPendingChildExecutionInfos() {
			p := &types.PendingChildExecutionInfo{
				WorkflowID:        ch.StartedWorkflowID,
				RunID:             ch.StartedRunID,
				WorkflowTypName:   ch.WorkflowTypeName,
				InitiatedID:       ch.InitiatedID,
				ParentClosePolicy: &ch.ParentClosePolicy,
			}
			result.PendingChildren = append(result.PendingChildren, p)
		}
	}

	if di, ok := mutableState.GetPendingDecision(); ok {
		pendingDecision := &types.PendingDecisionInfo{
			State:                      types.PendingDecisionStateScheduled.Ptr(),
			ScheduledTimestamp:         common.Int64Ptr(di.ScheduledTimestamp),
			Attempt:                    di.Attempt,
			OriginalScheduledTimestamp: common.Int64Ptr(di.OriginalScheduledTimestamp),
		}
		if di.StartedID != common.EmptyEventID {
			pendingDecision.State = types.PendingDecisionStateStarted.Ptr()
			pendingDecision.StartedTimestamp = common.Int64Ptr(di.StartedTimestamp)
		}
		result.PendingDecision = pendingDecision
	}

	return result, nil
}

func (e *historyEngineImpl) RecordActivityTaskStarted(
	ctx context.Context,
	request *types.RecordActivityTaskStartedRequest,
) (*types.RecordActivityTaskStartedResponse, error) {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(request.DomainUUID)
	if err != nil {
		return nil, err
	}

	domainInfo := domainEntry.GetInfo()

	domainID := domainInfo.ID
	domainName := domainInfo.Name

	workflowExecution := types.WorkflowExecution{
		WorkflowID: request.WorkflowExecution.WorkflowID,
		RunID:      request.WorkflowExecution.RunID,
	}

	response := &types.RecordActivityTaskStartedResponse{}
	err = workflow.UpdateWithAction(ctx, e.executionCache, domainID, workflowExecution, false, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				return workflow.ErrAlreadyCompleted
			}

			scheduleID := request.GetScheduleID()
			requestID := request.GetRequestID()
			ai, isRunning := mutableState.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= mutableState.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRecordActivityTaskStartedScope, metrics.StaleMutableStateCounter)
				e.logger.Error("Encounter stale mutable state in RecordActivityTaskStarted",
					tag.WorkflowDomainName(domainName),
					tag.WorkflowID(workflowExecution.GetWorkflowID()),
					tag.WorkflowRunID(workflowExecution.GetRunID()),
					tag.WorkflowScheduleID(scheduleID),
					tag.WorkflowNextEventID(mutableState.GetNextEventID()),
				)
				return workflow.ErrStaleState
			}

			// Check execution state to make sure task is in the list of outstanding tasks and it is not yet started.  If
			// task is not outstanding than it is most probably a duplicate and complete the task.
			if !isRunning {
				// Looks like ActivityTask already completed as a result of another call.
				// It is OK to drop the task at this point.
				e.logger.Debug("Potentially duplicate task.", tag.TaskID(request.GetTaskID()), tag.WorkflowScheduleID(scheduleID), tag.TaskType(persistence.TransferTaskTypeActivityTask))
				return workflow.ErrActivityTaskNotFound
			}

			scheduledEvent, err := mutableState.GetActivityScheduledEvent(ctx, scheduleID)
			if err != nil {
				return err
			}
			response.ScheduledEvent = scheduledEvent
			response.ScheduledTimestampOfThisAttempt = common.Int64Ptr(ai.ScheduledTime.UnixNano())

			response.Attempt = int64(ai.Attempt)
			response.HeartbeatDetails = ai.Details

			response.WorkflowType = mutableState.GetWorkflowType()
			response.WorkflowDomain = domainName

			if ai.StartedID != common.EmptyEventID {
				// If activity is started as part of the current request scope then return a positive response
				if ai.RequestID == requestID {
					response.StartedTimestamp = common.Int64Ptr(ai.StartedTime.UnixNano())
					return nil
				}

				// Looks like ActivityTask already started as a result of another call.
				// It is OK to drop the task at this point.
				e.logger.Debug("Potentially duplicate task.", tag.TaskID(request.GetTaskID()), tag.WorkflowScheduleID(scheduleID), tag.TaskType(persistence.TransferTaskTypeActivityTask))
				return &types.EventAlreadyStartedError{Message: "Activity task already started."}
			}

			if _, err := mutableState.AddActivityTaskStartedEvent(
				ai, scheduleID, requestID, request.PollRequest.GetIdentity(),
			); err != nil {
				return err
			}

			response.StartedTimestamp = common.Int64Ptr(ai.StartedTime.UnixNano())

			return nil
		})

	if err != nil {
		return nil, err
	}

	return response, err
}

// ScheduleDecisionTask schedules a decision if no outstanding decision found
func (e *historyEngineImpl) ScheduleDecisionTask(
	ctx context.Context,
	req *types.ScheduleDecisionTaskRequest,
) error {
	return e.decisionHandler.HandleDecisionTaskScheduled(ctx, req)
}

// RecordDecisionTaskStarted starts a decision
func (e *historyEngineImpl) RecordDecisionTaskStarted(
	ctx context.Context,
	request *types.RecordDecisionTaskStartedRequest,
) (*types.RecordDecisionTaskStartedResponse, error) {
	return e.decisionHandler.HandleDecisionTaskStarted(ctx, request)
}

// RespondDecisionTaskCompleted completes a decision task
func (e *historyEngineImpl) RespondDecisionTaskCompleted(
	ctx context.Context,
	req *types.HistoryRespondDecisionTaskCompletedRequest,
) (*types.HistoryRespondDecisionTaskCompletedResponse, error) {
	return e.decisionHandler.HandleDecisionTaskCompleted(ctx, req)
}

// RespondDecisionTaskFailed fails a decision
func (e *historyEngineImpl) RespondDecisionTaskFailed(
	ctx context.Context,
	req *types.HistoryRespondDecisionTaskFailedRequest,
) error {
	return e.decisionHandler.HandleDecisionTaskFailed(ctx, req)
}

// RespondActivityTaskCompleted completes an activity task.
func (e *historyEngineImpl) RespondActivityTaskCompleted(
	ctx context.Context,
	req *types.HistoryRespondActivityTaskCompletedRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(req.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID
	domainName := domainEntry.GetInfo().Name

	request := req.CompleteRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return workflow.ErrDeserializingToken
	}

	workflowExecution := types.WorkflowExecution{
		WorkflowID: token.WorkflowID,
		RunID:      token.RunID,
	}

	var activityStartedTime time.Time
	var taskList string
	err = workflow.UpdateWithAction(ctx, e.executionCache, domainID, workflowExecution, true, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				return workflow.ErrAlreadyCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call CompleteActivityById, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, mutableState)
				if err0 != nil {
					return err0
				}
			}
			ai, isRunning := mutableState.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= mutableState.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRespondActivityTaskCompletedScope, metrics.StaleMutableStateCounter)
				e.logger.Error("Encounter stale mutable state in RecordActivityTaskCompleted",
					tag.WorkflowDomainName(domainName),
					tag.WorkflowID(workflowExecution.GetWorkflowID()),
					tag.WorkflowRunID(workflowExecution.GetRunID()),
					tag.WorkflowScheduleID(scheduleID),
					tag.WorkflowNextEventID(mutableState.GetNextEventID()),
				)
				return workflow.ErrStaleState
			}

			if !isRunning || ai.StartedID == common.EmptyEventID ||
				(token.ScheduleID != common.EmptyEventID && token.ScheduleAttempt != int64(ai.Attempt)) {
				return workflow.ErrActivityTaskNotFound
			}

			if _, err := mutableState.AddActivityTaskCompletedEvent(scheduleID, ai.StartedID, request); err != nil {
				// Unable to add ActivityTaskCompleted event to history
				return &types.InternalServiceError{Message: "Unable to add ActivityTaskCompleted event to history."}
			}
			activityStartedTime = ai.StartedTime
			taskList = ai.TaskList
			return nil
		})
	if err == nil && !activityStartedTime.IsZero() {
		scope := e.metricsClient.Scope(metrics.HistoryRespondActivityTaskCompletedScope).
			Tagged(
				metrics.DomainTag(domainName),
				metrics.WorkflowTypeTag(token.WorkflowType),
				metrics.ActivityTypeTag(token.ActivityType),
				metrics.TaskListTag(taskList),
			)
		scope.RecordTimer(metrics.ActivityE2ELatency, time.Since(activityStartedTime))
	}
	return err
}

// RespondActivityTaskFailed completes an activity task failure.
func (e *historyEngineImpl) RespondActivityTaskFailed(
	ctx context.Context,
	req *types.HistoryRespondActivityTaskFailedRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(req.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID
	domainName := domainEntry.GetInfo().Name

	request := req.FailedRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return workflow.ErrDeserializingToken
	}

	workflowExecution := types.WorkflowExecution{
		WorkflowID: token.WorkflowID,
		RunID:      token.RunID,
	}

	var activityStartedTime time.Time
	var taskList string
	err = workflow.UpdateWithActionFunc(
		ctx,
		e.executionCache,
		domainID,
		workflowExecution,
		e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) (*workflow.UpdateAction, error) {
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, workflow.ErrAlreadyCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call CompleteActivityById, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, mutableState)
				if err0 != nil {
					return nil, err0
				}
			}
			ai, isRunning := mutableState.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= mutableState.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRespondActivityTaskFailedScope, metrics.StaleMutableStateCounter)
				e.logger.Error("Encounter stale mutable state in RecordActivityTaskFailed",
					tag.WorkflowDomainName(domainName),
					tag.WorkflowID(workflowExecution.GetWorkflowID()),
					tag.WorkflowRunID(workflowExecution.GetRunID()),
					tag.WorkflowScheduleID(scheduleID),
					tag.WorkflowNextEventID(mutableState.GetNextEventID()),
				)
				return nil, workflow.ErrStaleState
			}

			if !isRunning || ai.StartedID == common.EmptyEventID ||
				(token.ScheduleID != common.EmptyEventID && token.ScheduleAttempt != int64(ai.Attempt)) {
				return nil, workflow.ErrActivityTaskNotFound
			}

			postActions := &workflow.UpdateAction{}
			ok, err := mutableState.RetryActivity(ai, req.FailedRequest.GetReason(), req.FailedRequest.GetDetails())
			if err != nil {
				return nil, err
			}
			if !ok {
				// no more retry, and we want to record the failure event
				if _, err := mutableState.AddActivityTaskFailedEvent(scheduleID, ai.StartedID, request); err != nil {
					// Unable to add ActivityTaskFailed event to history
					return nil, &types.InternalServiceError{Message: "Unable to add ActivityTaskFailed event to history."}
				}
				postActions.CreateDecision = true
			}

			activityStartedTime = ai.StartedTime
			taskList = ai.TaskList
			return postActions, nil
		},
	)
	if err == nil && !activityStartedTime.IsZero() {
		scope := e.metricsClient.Scope(metrics.HistoryRespondActivityTaskFailedScope).
			Tagged(
				metrics.DomainTag(domainName),
				metrics.WorkflowTypeTag(token.WorkflowType),
				metrics.ActivityTypeTag(token.ActivityType),
				metrics.TaskListTag(taskList),
			)
		scope.RecordTimer(metrics.ActivityE2ELatency, time.Since(activityStartedTime))
	}
	return err
}

// RespondActivityTaskCanceled completes an activity task failure.
func (e *historyEngineImpl) RespondActivityTaskCanceled(
	ctx context.Context,
	req *types.HistoryRespondActivityTaskCanceledRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(req.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID
	domainName := domainEntry.GetInfo().Name

	request := req.CancelRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return workflow.ErrDeserializingToken
	}

	workflowExecution := types.WorkflowExecution{
		WorkflowID: token.WorkflowID,
		RunID:      token.RunID,
	}

	var activityStartedTime time.Time
	var taskList string
	err = workflow.UpdateWithAction(ctx, e.executionCache, domainID, workflowExecution, true, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				return workflow.ErrAlreadyCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call CompleteActivityById, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, mutableState)
				if err0 != nil {
					return err0
				}
			}
			ai, isRunning := mutableState.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= mutableState.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRespondActivityTaskCanceledScope, metrics.StaleMutableStateCounter)
				e.logger.Error("Encounter stale mutable state in RecordActivityTaskCanceled",
					tag.WorkflowDomainName(domainName),
					tag.WorkflowID(workflowExecution.GetWorkflowID()),
					tag.WorkflowRunID(workflowExecution.GetRunID()),
					tag.WorkflowScheduleID(scheduleID),
					tag.WorkflowNextEventID(mutableState.GetNextEventID()),
				)
				return workflow.ErrStaleState
			}

			if !isRunning || ai.StartedID == common.EmptyEventID ||
				(token.ScheduleID != common.EmptyEventID && token.ScheduleAttempt != int64(ai.Attempt)) {
				return workflow.ErrActivityTaskNotFound
			}

			if _, err := mutableState.AddActivityTaskCanceledEvent(
				scheduleID,
				ai.StartedID,
				ai.CancelRequestID,
				request.Details,
				request.Identity); err != nil {
				// Unable to add ActivityTaskCanceled event to history
				return &types.InternalServiceError{Message: "Unable to add ActivityTaskCanceled event to history."}
			}

			activityStartedTime = ai.StartedTime
			taskList = ai.TaskList
			return nil
		})
	if err == nil && !activityStartedTime.IsZero() {
		scope := e.metricsClient.Scope(metrics.HistoryClientRespondActivityTaskCanceledScope).
			Tagged(
				metrics.DomainTag(domainName),
				metrics.WorkflowTypeTag(token.WorkflowType),
				metrics.ActivityTypeTag(token.ActivityType),
				metrics.TaskListTag(taskList),
			)
		scope.RecordTimer(metrics.ActivityE2ELatency, time.Since(activityStartedTime))
	}
	return err
}

// RecordActivityTaskHeartbeat records an heartbeat for a task.
// This method can be used for two purposes.
// - For reporting liveness of the activity.
// - For reporting progress of the activity, this can be done even if the liveness is not configured.
func (e *historyEngineImpl) RecordActivityTaskHeartbeat(
	ctx context.Context,
	req *types.HistoryRecordActivityTaskHeartbeatRequest,
) (*types.RecordActivityTaskHeartbeatResponse, error) {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(req.DomainUUID)
	if err != nil {
		return nil, err
	}
	domainID := domainEntry.GetInfo().ID

	request := req.HeartbeatRequest
	token, err0 := e.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return nil, workflow.ErrDeserializingToken
	}

	workflowExecution := types.WorkflowExecution{
		WorkflowID: token.WorkflowID,
		RunID:      token.RunID,
	}

	var cancelRequested bool
	err = workflow.UpdateWithAction(ctx, e.executionCache, domainID, workflowExecution, false, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				e.logger.Debug("Heartbeat failed")
				return workflow.ErrAlreadyCompleted
			}

			scheduleID := token.ScheduleID
			if scheduleID == common.EmptyEventID { // client call RecordActivityHeartbeatByID, so get scheduleID by activityID
				scheduleID, err0 = getScheduleID(token.ActivityID, mutableState)
				if err0 != nil {
					return err0
				}
			}
			ai, isRunning := mutableState.GetActivityInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= mutableState.GetNextEventID() {
				e.metricsClient.IncCounter(metrics.HistoryRecordActivityTaskHeartbeatScope, metrics.StaleMutableStateCounter)
				e.logger.Error("Encounter stale mutable state in RecordActivityTaskHeartbeat",
					tag.WorkflowDomainName(domainEntry.GetInfo().Name),
					tag.WorkflowID(workflowExecution.GetWorkflowID()),
					tag.WorkflowRunID(workflowExecution.GetRunID()),
					tag.WorkflowScheduleID(scheduleID),
					tag.WorkflowNextEventID(mutableState.GetNextEventID()),
				)
				return workflow.ErrStaleState
			}

			if !isRunning || ai.StartedID == common.EmptyEventID ||
				(token.ScheduleID != common.EmptyEventID && token.ScheduleAttempt != int64(ai.Attempt)) {
				return workflow.ErrActivityTaskNotFound
			}

			cancelRequested = ai.CancelRequested

			e.logger.Debug(fmt.Sprintf("Activity HeartBeat: scheduleEventID: %v, ActivityInfo: %+v, CancelRequested: %v",
				scheduleID, ai, cancelRequested))

			// Save progress and last HB reported time.
			mutableState.UpdateActivityProgress(ai, request)

			return nil
		})

	if err != nil {
		return &types.RecordActivityTaskHeartbeatResponse{}, err
	}

	return &types.RecordActivityTaskHeartbeatResponse{CancelRequested: cancelRequested}, nil
}

// RequestCancelWorkflowExecution records request cancellation event for workflow execution
func (e *historyEngineImpl) RequestCancelWorkflowExecution(
	ctx context.Context,
	req *types.HistoryRequestCancelWorkflowExecutionRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(req.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID

	request := req.CancelRequest
	parentExecution := req.ExternalWorkflowExecution
	childWorkflowOnly := req.GetChildWorkflowOnly()
	workflowExecution := types.WorkflowExecution{
		WorkflowID: request.WorkflowExecution.WorkflowID,
		RunID:      request.WorkflowExecution.RunID,
	}

	return workflow.UpdateCurrentWithActionFunc(ctx, e.executionCache, e.executionManager, domainID, workflowExecution, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) (*workflow.UpdateAction, error) {
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, workflow.ErrAlreadyCompleted
			}

			executionInfo := mutableState.GetExecutionInfo()
			if childWorkflowOnly {
				parentWorkflowID := executionInfo.ParentWorkflowID
				parentRunID := executionInfo.ParentRunID
				if parentExecution.GetWorkflowID() != parentWorkflowID ||
					parentExecution.GetRunID() != parentRunID {
					return nil, workflow.ErrParentMismatch
				}
			}

			isCancelRequested, cancelRequestID := mutableState.IsCancelRequested()
			if isCancelRequested {
				cancelRequest := req.CancelRequest
				if cancelRequest.RequestID != "" && cancelRequest.RequestID == cancelRequestID {
					return workflow.UpdateWithNewDecision, nil
				}
				// if we consider workflow cancellation idempotent, then this error is redundant
				// this error maybe useful if this API is invoked by external, not decision from transfer queue
				return nil, workflow.ErrCancellationAlreadyRequested
			}

			if _, err := mutableState.AddWorkflowExecutionCancelRequestedEvent("", req); err != nil {
				return nil, &types.InternalServiceError{Message: "Unable to cancel workflow execution."}
			}

			return workflow.UpdateWithNewDecision, nil
		})
}

func (e *historyEngineImpl) SignalWorkflowExecution(
	ctx context.Context,
	signalRequest *types.HistorySignalWorkflowExecutionRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(signalRequest.DomainUUID)
	if err != nil {
		return err
	}
	if domainEntry.GetInfo().Status != persistence.DomainStatusRegistered {
		return errDomainDeprecated
	}
	domainID := domainEntry.GetInfo().ID

	request := signalRequest.SignalRequest
	parentExecution := signalRequest.ExternalWorkflowExecution
	childWorkflowOnly := signalRequest.GetChildWorkflowOnly()
	workflowExecution := types.WorkflowExecution{
		WorkflowID: request.WorkflowExecution.WorkflowID,
		RunID:      request.WorkflowExecution.RunID,
	}

	return workflow.UpdateCurrentWithActionFunc(
		ctx,
		e.executionCache,
		e.executionManager,
		domainID,
		workflowExecution,
		e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) (*workflow.UpdateAction, error) {
			executionInfo := mutableState.GetExecutionInfo()
			createDecisionTask := true
			// Do not create decision task when the workflow is cron and the cron has not been started yet
			if mutableState.GetExecutionInfo().CronSchedule != "" && !mutableState.HasProcessedOrPendingDecision() {
				createDecisionTask = false
			}
			postActions := &workflow.UpdateAction{
				CreateDecision: createDecisionTask,
			}

			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, workflow.ErrAlreadyCompleted
			}

			maxAllowedSignals := e.config.MaximumSignalsPerExecution(domainEntry.GetInfo().Name)
			if maxAllowedSignals > 0 && int(executionInfo.SignalCount) >= maxAllowedSignals {
				e.logger.Info("Execution limit reached for maximum signals", tag.WorkflowSignalCount(executionInfo.SignalCount),
					tag.WorkflowID(workflowExecution.GetWorkflowID()),
					tag.WorkflowRunID(workflowExecution.GetRunID()),
					tag.WorkflowDomainID(domainID))
				return nil, workflow.ErrSignalsLimitExceeded
			}

			if childWorkflowOnly {
				parentWorkflowID := executionInfo.ParentWorkflowID
				parentRunID := executionInfo.ParentRunID
				if parentExecution.GetWorkflowID() != parentWorkflowID ||
					parentExecution.GetRunID() != parentRunID {
					return nil, workflow.ErrParentMismatch
				}
			}

			// deduplicate by request id for signal decision
			if requestID := request.GetRequestID(); requestID != "" {
				if mutableState.IsSignalRequested(requestID) {
					return postActions, nil
				}
				mutableState.AddSignalRequested(requestID)
			}

			if _, err := mutableState.AddWorkflowExecutionSignaled(
				request.GetSignalName(),
				request.GetInput(),
				request.GetIdentity()); err != nil {
				return nil, &types.InternalServiceError{Message: "Unable to signal workflow execution."}
			}

			return postActions, nil
		})
}

func (e *historyEngineImpl) SignalWithStartWorkflowExecution(
	ctx context.Context,
	signalWithStartRequest *types.HistorySignalWithStartWorkflowExecutionRequest,
) (retResp *types.StartWorkflowExecutionResponse, retError error) {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(signalWithStartRequest.DomainUUID)
	if err != nil {
		return nil, err
	}
	if domainEntry.GetInfo().Status != persistence.DomainStatusRegistered {
		return nil, errDomainDeprecated
	}
	domainID := domainEntry.GetInfo().ID

	sRequest := signalWithStartRequest.SignalWithStartRequest
	workflowExecution := types.WorkflowExecution{
		WorkflowID: sRequest.WorkflowID,
	}

	var prevMutableState execution.MutableState
	attempt := 0

	wfContext, release, err0 := e.executionCache.GetOrCreateWorkflowExecution(ctx, domainID, workflowExecution)

	if err0 == nil {
		defer func() { release(retError) }()
	Just_Signal_Loop:
		for ; attempt < workflow.ConditionalRetryCount; attempt++ {
			// workflow not exist, will create workflow then signal
			mutableState, err1 := wfContext.LoadWorkflowExecution(ctx)
			if err1 != nil {
				if _, ok := err1.(*types.EntityNotExistsError); ok {
					break
				}
				return nil, err1
			}
			// workflow exist but not running, will restart workflow then signal
			if !mutableState.IsWorkflowExecutionRunning() {
				prevMutableState = mutableState
				break
			}
			// workflow is running, if policy is TerminateIfRunning, terminate current run then signalWithStart
			if sRequest.GetWorkflowIDReusePolicy() == types.WorkflowIDReusePolicyTerminateIfRunning {
				workflowExecution.RunID = uuid.New()
				runningWFCtx := workflow.NewContext(wfContext, release, mutableState)
				return e.terminateAndStartWorkflow(
					ctx,
					runningWFCtx,
					workflowExecution,
					domainEntry,
					domainID,
					nil,
					signalWithStartRequest,
				)
			}

			executionInfo := mutableState.GetExecutionInfo()
			maxAllowedSignals := e.config.MaximumSignalsPerExecution(domainEntry.GetInfo().Name)
			if maxAllowedSignals > 0 && int(executionInfo.SignalCount) >= maxAllowedSignals {
				e.logger.Info("Execution limit reached for maximum signals", tag.WorkflowSignalCount(executionInfo.SignalCount),
					tag.WorkflowID(workflowExecution.GetWorkflowID()),
					tag.WorkflowRunID(workflowExecution.GetRunID()),
					tag.WorkflowDomainID(domainID))
				return nil, workflow.ErrSignalsLimitExceeded
			}

			if _, err := mutableState.AddWorkflowExecutionSignaled(
				sRequest.GetSignalName(),
				sRequest.GetSignalInput(),
				sRequest.GetIdentity()); err != nil {
				return nil, &types.InternalServiceError{Message: "Unable to signal workflow execution."}
			}

			// Create a transfer task to schedule a decision task
			if !mutableState.HasPendingDecision() {
				_, err := mutableState.AddDecisionTaskScheduledEvent(false)
				if err != nil {
					return nil, &types.InternalServiceError{Message: "Failed to add decision scheduled event."}
				}
			}

			// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict then reload
			// the history and try the operation again.
			if err := wfContext.UpdateWorkflowExecutionAsActive(ctx, e.shard.GetTimeSource().Now()); err != nil {
				if err == execution.ErrConflict {
					continue Just_Signal_Loop
				}
				return nil, err
			}
			return &types.StartWorkflowExecutionResponse{RunID: wfContext.GetExecution().RunID}, nil
		} // end for Just_Signal_Loop
		if attempt == workflow.ConditionalRetryCount {
			return nil, workflow.ErrMaxAttemptsExceeded
		}
	} else {
		if _, ok := err0.(*types.EntityNotExistsError); !ok {
			return nil, err0
		}
		// workflow not exist, will create workflow then signal
	}

	// Start workflow and signal
	startRequest, err := getStartRequest(domainID, sRequest)
	if err != nil {
		return nil, err
	}

	sigWithStartArg := &signalWithStartArg{
		signalWithStartRequest: signalWithStartRequest,
		prevMutableState:       prevMutableState,
	}
	return e.startWorkflowHelper(
		ctx,
		startRequest,
		domainEntry,
		metrics.HistorySignalWithStartWorkflowExecutionScope,
		sigWithStartArg,
	)
}

// RemoveSignalMutableState remove the signal request id in signal_requested for deduplicate
func (e *historyEngineImpl) RemoveSignalMutableState(
	ctx context.Context,
	request *types.RemoveSignalMutableStateRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(request.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID

	workflowExecution := types.WorkflowExecution{
		WorkflowID: request.WorkflowExecution.WorkflowID,
		RunID:      request.WorkflowExecution.RunID,
	}

	return workflow.UpdateWithAction(ctx, e.executionCache, domainID, workflowExecution, false, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				return workflow.ErrAlreadyCompleted
			}

			mutableState.DeleteSignalRequested(request.GetRequestID())

			return nil
		})
}

func (e *historyEngineImpl) TerminateWorkflowExecution(
	ctx context.Context,
	terminateRequest *types.HistoryTerminateWorkflowExecutionRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(terminateRequest.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID

	request := terminateRequest.TerminateRequest
	workflowExecution := types.WorkflowExecution{
		WorkflowID: request.WorkflowExecution.WorkflowID,
		RunID:      request.WorkflowExecution.RunID,
	}

	return workflow.UpdateCurrentWithActionFunc(
		ctx,
		e.executionCache,
		e.executionManager,
		domainID,
		workflowExecution,
		e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) (*workflow.UpdateAction, error) {
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, workflow.ErrAlreadyCompleted
			}

			eventBatchFirstEventID := mutableState.GetNextEventID()
			return workflow.UpdateWithoutDecision, execution.TerminateWorkflow(
				mutableState,
				eventBatchFirstEventID,
				request.GetReason(),
				request.GetDetails(),
				request.GetIdentity(),
			)
		})
}

// RecordChildExecutionCompleted records the completion of child execution into parent execution history
func (e *historyEngineImpl) RecordChildExecutionCompleted(
	ctx context.Context,
	completionRequest *types.RecordChildExecutionCompletedRequest,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(completionRequest.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID

	workflowExecution := types.WorkflowExecution{
		WorkflowID: completionRequest.WorkflowExecution.WorkflowID,
		RunID:      completionRequest.WorkflowExecution.RunID,
	}

	return workflow.UpdateWithAction(ctx, e.executionCache, domainID, workflowExecution, true, e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				return workflow.ErrAlreadyCompleted
			}

			initiatedID := completionRequest.InitiatedID
			completedExecution := completionRequest.CompletedExecution
			completionEvent := completionRequest.CompletionEvent

			// Check mutable state to make sure child execution is in pending child executions
			ci, isRunning := mutableState.GetChildExecutionInfo(initiatedID)
			if !isRunning || ci.StartedID == common.EmptyEventID {
				return &types.EntityNotExistsError{Message: "Pending child execution not found."}
			}
			if ci.StartedWorkflowID != completedExecution.GetWorkflowID() {
				return &types.EntityNotExistsError{Message: "Pending child execution not found."}
			}

			switch *completionEvent.EventType {
			case types.EventTypeWorkflowExecutionCompleted:
				attributes := completionEvent.WorkflowExecutionCompletedEventAttributes
				_, err = mutableState.AddChildWorkflowExecutionCompletedEvent(initiatedID, completedExecution, attributes)
			case types.EventTypeWorkflowExecutionFailed:
				attributes := completionEvent.WorkflowExecutionFailedEventAttributes
				_, err = mutableState.AddChildWorkflowExecutionFailedEvent(initiatedID, completedExecution, attributes)
			case types.EventTypeWorkflowExecutionCanceled:
				attributes := completionEvent.WorkflowExecutionCanceledEventAttributes
				_, err = mutableState.AddChildWorkflowExecutionCanceledEvent(initiatedID, completedExecution, attributes)
			case types.EventTypeWorkflowExecutionTerminated:
				attributes := completionEvent.WorkflowExecutionTerminatedEventAttributes
				_, err = mutableState.AddChildWorkflowExecutionTerminatedEvent(initiatedID, completedExecution, attributes)
			case types.EventTypeWorkflowExecutionTimedOut:
				attributes := completionEvent.WorkflowExecutionTimedOutEventAttributes
				_, err = mutableState.AddChildWorkflowExecutionTimedOutEvent(initiatedID, completedExecution, attributes)
			}

			return err
		})
}

func (e *historyEngineImpl) ReplicateEventsV2(
	ctx context.Context,
	replicateRequest *types.ReplicateEventsV2Request,
) error {

	return e.nDCReplicator.ApplyEvents(ctx, replicateRequest)
}

func (e *historyEngineImpl) SyncShardStatus(
	ctx context.Context,
	request *types.SyncShardStatusRequest,
) error {

	clusterName := request.GetSourceCluster()
	now := time.Unix(0, request.GetTimestamp())

	// here there are 3 main things
	// 1. update the view of remote cluster's shard time
	// 2. notify the timer gate in the timer queue standby processor
	// 3, notify the transfer (essentially a no op, just put it here so it looks symmetric)
	e.shard.SetCurrentTime(clusterName, now)
	e.txProcessor.NotifyNewTask(clusterName, nil, []persistence.Task{})
	e.timerProcessor.NotifyNewTask(clusterName, nil, []persistence.Task{})
	return nil
}

func (e *historyEngineImpl) SyncActivity(
	ctx context.Context,
	request *types.SyncActivityRequest,
) (retError error) {

	return e.nDCActivityReplicator.SyncActivity(ctx, request)
}

func (e *historyEngineImpl) ResetWorkflowExecution(
	ctx context.Context,
	resetRequest *types.HistoryResetWorkflowExecutionRequest,
) (response *types.ResetWorkflowExecutionResponse, retError error) {

	request := resetRequest.ResetRequest
	domainID := resetRequest.GetDomainUUID()
	workflowID := request.WorkflowExecution.GetWorkflowID()
	baseRunID := request.WorkflowExecution.GetRunID()

	baseContext, baseReleaseFn, err := e.executionCache.GetOrCreateWorkflowExecution(
		ctx,
		domainID,
		types.WorkflowExecution{
			WorkflowID: workflowID,
			RunID:      baseRunID,
		},
	)
	if err != nil {
		return nil, err
	}
	defer func() { baseReleaseFn(retError) }()

	baseMutableState, err := baseContext.LoadWorkflowExecution(ctx)
	if err != nil {
		return nil, err
	}
	if ok := baseMutableState.HasProcessedOrPendingDecision(); !ok {
		return nil, &types.BadRequestError{
			Message: "Cannot reset workflow without a decision task schedule.",
		}
	}
	if request.GetDecisionFinishEventID() <= common.FirstEventID ||
		request.GetDecisionFinishEventID() > baseMutableState.GetNextEventID() {
		return nil, &types.BadRequestError{
			Message: "Decision finish ID must be > 1 && <= workflow next event ID.",
		}
	}

	// also load the current run of the workflow, it can be different from the base runID
	resp, err := e.executionManager.GetCurrentExecution(ctx, &persistence.GetCurrentExecutionRequest{
		DomainID:   domainID,
		WorkflowID: request.WorkflowExecution.GetWorkflowID(),
	})
	if err != nil {
		return nil, err
	}

	currentRunID := resp.RunID
	var currentContext execution.Context
	var currentMutableState execution.MutableState
	var currentReleaseFn execution.ReleaseFunc
	if currentRunID == baseRunID {
		currentContext = baseContext
		currentMutableState = baseMutableState
	} else {
		currentContext, currentReleaseFn, err = e.executionCache.GetOrCreateWorkflowExecution(
			ctx,
			domainID,
			types.WorkflowExecution{
				WorkflowID: workflowID,
				RunID:      currentRunID,
			},
		)
		if err != nil {
			return nil, err
		}
		defer func() { currentReleaseFn(retError) }()

		currentMutableState, err = currentContext.LoadWorkflowExecution(ctx)
		if err != nil {
			return nil, err
		}
	}

	// dedup by requestID
	if currentMutableState.GetExecutionInfo().CreateRequestID == request.GetRequestID() {
		e.logger.Info("Duplicated reset request",
			tag.WorkflowID(workflowID),
			tag.WorkflowRunID(currentRunID),
			tag.WorkflowDomainID(domainID))
		return &types.ResetWorkflowExecutionResponse{
			RunID: currentRunID,
		}, nil
	}

	resetRunID := uuid.New()
	baseRebuildLastEventID := request.GetDecisionFinishEventID() - 1
	baseVersionHistories := baseMutableState.GetVersionHistories()
	baseCurrentBranchToken, err := baseMutableState.GetCurrentBranchToken()
	if err != nil {
		return nil, err
	}
	baseRebuildLastEventVersion := baseMutableState.GetCurrentVersion()
	baseNextEventID := baseMutableState.GetNextEventID()

	if baseVersionHistories != nil {
		baseCurrentVersionHistory, err := baseVersionHistories.GetCurrentVersionHistory()
		if err != nil {
			return nil, err
		}
		baseRebuildLastEventVersion, err = baseCurrentVersionHistory.GetEventVersion(baseRebuildLastEventID)
		if err != nil {
			return nil, err
		}
		baseCurrentBranchToken = baseCurrentVersionHistory.GetBranchToken()
	}

	if err := e.workflowResetter.ResetWorkflow(
		ctx,
		domainID,
		workflowID,
		baseRunID,
		baseCurrentBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		baseNextEventID,
		resetRunID,
		request.GetRequestID(),
		execution.NewWorkflow(
			ctx,
			e.shard.GetDomainCache(),
			e.shard.GetClusterMetadata(),
			currentContext,
			currentMutableState,
			currentReleaseFn,
		),
		request.GetReason(),
		nil,
		request.GetSkipSignalReapply(),
	); err != nil {
		return nil, err
	}
	return &types.ResetWorkflowExecutionResponse{
		RunID: resetRunID,
	}, nil
}

func (e *historyEngineImpl) NotifyNewHistoryEvent(
	event *events.Notification,
) {

	e.historyEventNotifier.NotifyNewHistoryEvent(event)
}

func (e *historyEngineImpl) NotifyNewTransferTasks(
	executionInfo *persistence.WorkflowExecutionInfo,
	tasks []persistence.Task,
) {

	if len(tasks) > 0 {
		task := tasks[0]
		clusterName := e.clusterMetadata.ClusterNameForFailoverVersion(task.GetVersion())
		e.txProcessor.NotifyNewTask(clusterName, executionInfo, tasks)
	}
}

func (e *historyEngineImpl) NotifyNewTimerTasks(
	executionInfo *persistence.WorkflowExecutionInfo,
	tasks []persistence.Task,
) {

	if len(tasks) > 0 {
		task := tasks[0]
		clusterName := e.clusterMetadata.ClusterNameForFailoverVersion(task.GetVersion())
		e.timerProcessor.NotifyNewTask(clusterName, executionInfo, tasks)
	}
}

func (e *historyEngineImpl) ResetTransferQueue(
	ctx context.Context,
	clusterName string,
) error {
	_, err := e.txProcessor.HandleAction(clusterName, queue.NewResetAction())
	return err
}

func (e *historyEngineImpl) ResetTimerQueue(
	ctx context.Context,
	clusterName string,
) error {
	_, err := e.timerProcessor.HandleAction(clusterName, queue.NewResetAction())
	return err
}

func (e *historyEngineImpl) DescribeTransferQueue(
	ctx context.Context,
	clusterName string,
) (*types.DescribeQueueResponse, error) {
	return e.describeQueue(e.txProcessor, clusterName)
}

func (e *historyEngineImpl) DescribeTimerQueue(
	ctx context.Context,
	clusterName string,
) (*types.DescribeQueueResponse, error) {
	return e.describeQueue(e.timerProcessor, clusterName)
}

func (e *historyEngineImpl) describeQueue(
	queueProcessor queue.Processor,
	clusterName string,
) (*types.DescribeQueueResponse, error) {
	resp, err := queueProcessor.HandleAction(clusterName, queue.NewGetStateAction())
	if err != nil {
		return nil, err
	}

	serializedStates := make([]string, 0, len(resp.GetStateActionResult.States))
	for _, state := range resp.GetStateActionResult.States {
		serializedStates = append(serializedStates, e.serializeQueueState(state))
	}
	return &types.DescribeQueueResponse{
		ProcessingQueueStates: serializedStates,
	}, nil
}

func (e *historyEngineImpl) serializeQueueState(
	state queue.ProcessingQueueState,
) string {
	return fmt.Sprintf("%v", state)
}

func validateStartWorkflowExecutionRequest(
	request *types.StartWorkflowExecutionRequest,
	maxIDLengthLimit int,
) error {

	if len(request.GetRequestID()) == 0 {
		return &types.BadRequestError{Message: "Missing request ID."}
	}
	if request.ExecutionStartToCloseTimeoutSeconds == nil || request.GetExecutionStartToCloseTimeoutSeconds() <= 0 {
		return &types.BadRequestError{Message: "Missing or invalid ExecutionStartToCloseTimeoutSeconds."}
	}
	if request.TaskStartToCloseTimeoutSeconds == nil || request.GetTaskStartToCloseTimeoutSeconds() <= 0 {
		return &types.BadRequestError{Message: "Missing or invalid TaskStartToCloseTimeoutSeconds."}
	}
	if request.TaskList == nil || request.TaskList.GetName() == "" {
		return &types.BadRequestError{Message: "Missing Tasklist."}
	}
	if request.WorkflowType == nil || request.WorkflowType.GetName() == "" {
		return &types.BadRequestError{Message: "Missing WorkflowType."}
	}
	if len(request.GetDomain()) > maxIDLengthLimit {
		return &types.BadRequestError{Message: "Domain exceeds length limit."}
	}
	if len(request.GetWorkflowID()) > maxIDLengthLimit {
		return &types.BadRequestError{Message: "WorkflowId exceeds length limit."}
	}
	if len(request.TaskList.GetName()) > maxIDLengthLimit {
		return &types.BadRequestError{Message: "TaskList exceeds length limit."}
	}
	if len(request.WorkflowType.GetName()) > maxIDLengthLimit {
		return &types.BadRequestError{Message: "WorkflowType exceeds length limit."}
	}

	return common.ValidateRetryPolicy(request.RetryPolicy)
}

func (e *historyEngineImpl) overrideStartWorkflowExecutionRequest(
	domainEntry *cache.DomainCacheEntry,
	request *types.StartWorkflowExecutionRequest,
	metricsScope int,
) {

	domainName := domainEntry.GetInfo().Name
	maxDecisionStartToCloseTimeoutSeconds := int32(e.config.MaxDecisionStartToCloseSeconds(domainName))

	taskStartToCloseTimeoutSecs := request.GetTaskStartToCloseTimeoutSeconds()
	taskStartToCloseTimeoutSecs = common.MinInt32(taskStartToCloseTimeoutSecs, maxDecisionStartToCloseTimeoutSeconds)
	taskStartToCloseTimeoutSecs = common.MinInt32(taskStartToCloseTimeoutSecs, request.GetExecutionStartToCloseTimeoutSeconds())

	if taskStartToCloseTimeoutSecs != request.GetTaskStartToCloseTimeoutSeconds() {
		request.TaskStartToCloseTimeoutSeconds = &taskStartToCloseTimeoutSecs
		e.metricsClient.Scope(
			metricsScope,
			metrics.DomainTag(domainName),
		).IncCounter(metrics.DecisionStartToCloseTimeoutOverrideCount)
	}
}

func getScheduleID(
	activityID string,
	mutableState execution.MutableState,
) (int64, error) {

	if activityID == "" {
		return 0, &types.BadRequestError{Message: "Neither ActivityID nor ScheduleID is provided"}
	}
	activityInfo, ok := mutableState.GetActivityByActivityID(activityID)
	if !ok {
		return 0, &types.BadRequestError{Message: "Cannot locate Activity ScheduleID"}
	}
	return activityInfo.ScheduleID, nil
}

func getStartRequest(
	domainID string,
	request *types.SignalWithStartWorkflowExecutionRequest,
) (*types.HistoryStartWorkflowExecutionRequest, error) {

	req := &types.StartWorkflowExecutionRequest{
		Domain:                              request.Domain,
		WorkflowID:                          request.WorkflowID,
		WorkflowType:                        request.WorkflowType,
		TaskList:                            request.TaskList,
		Input:                               request.Input,
		ExecutionStartToCloseTimeoutSeconds: request.ExecutionStartToCloseTimeoutSeconds,
		TaskStartToCloseTimeoutSeconds:      request.TaskStartToCloseTimeoutSeconds,
		Identity:                            request.Identity,
		RequestID:                           request.RequestID,
		WorkflowIDReusePolicy:               request.WorkflowIDReusePolicy,
		RetryPolicy:                         request.RetryPolicy,
		CronSchedule:                        request.CronSchedule,
		Memo:                                request.Memo,
		SearchAttributes:                    request.SearchAttributes,
		Header:                              request.Header,
		DelayStartSeconds:                   request.DelayStartSeconds,
	}

	startRequest, err := common.CreateHistoryStartWorkflowRequest(domainID, req, time.Now())
	if err != nil {
		return nil, err
	}

	return startRequest, nil
}

func (e *historyEngineImpl) applyWorkflowIDReusePolicyForSigWithStart(
	prevExecutionInfo *persistence.WorkflowExecutionInfo,
	execution types.WorkflowExecution,
	wfIDReusePolicy types.WorkflowIDReusePolicy,
) error {

	prevStartRequestID := prevExecutionInfo.CreateRequestID
	prevRunID := prevExecutionInfo.RunID
	prevState := prevExecutionInfo.State
	prevCloseState := prevExecutionInfo.CloseStatus

	return e.applyWorkflowIDReusePolicyHelper(
		prevStartRequestID,
		prevRunID,
		prevState,
		prevCloseState,
		execution,
		wfIDReusePolicy,
	)
}

func (e *historyEngineImpl) applyWorkflowIDReusePolicyHelper(
	prevStartRequestID,
	prevRunID string,
	prevState int,
	prevCloseState int,
	execution types.WorkflowExecution,
	wfIDReusePolicy types.WorkflowIDReusePolicy,
) error {

	// here we know some information about the prev workflow, i.e. either running right now
	// or has history check if the workflow is finished
	switch prevState {
	case persistence.WorkflowStateCreated,
		persistence.WorkflowStateRunning:
		msg := "Workflow execution is already running. WorkflowId: %v, RunId: %v."
		return getWorkflowAlreadyStartedError(msg, prevStartRequestID, execution.GetWorkflowID(), prevRunID)
	case persistence.WorkflowStateCompleted:
		// previous workflow completed, proceed
	default:
		// persistence.WorkflowStateZombie or unknown type
		return &types.InternalServiceError{Message: fmt.Sprintf("Failed to process workflow, workflow has invalid state: %v.", prevState)}
	}

	switch wfIDReusePolicy {
	case types.WorkflowIDReusePolicyAllowDuplicateFailedOnly:
		if _, ok := FailedWorkflowCloseState[prevCloseState]; !ok {
			msg := "Workflow execution already finished successfully. WorkflowId: %v, RunId: %v. Workflow ID reuse policy: allow duplicate workflow ID if last run failed."
			return getWorkflowAlreadyStartedError(msg, prevStartRequestID, execution.GetWorkflowID(), prevRunID)
		}
	case types.WorkflowIDReusePolicyAllowDuplicate,
		types.WorkflowIDReusePolicyTerminateIfRunning:
		// no check need here
	case types.WorkflowIDReusePolicyRejectDuplicate:
		msg := "Workflow execution already finished. WorkflowId: %v, RunId: %v. Workflow ID reuse policy: reject duplicate workflow ID."
		return getWorkflowAlreadyStartedError(msg, prevStartRequestID, execution.GetWorkflowID(), prevRunID)
	default:
		return &types.InternalServiceError{Message: "Failed to process start workflow reuse policy."}
	}

	return nil
}

func getWorkflowAlreadyStartedError(errMsg string, createRequestID string, workflowID string, runID string) error {
	return &types.WorkflowExecutionAlreadyStartedError{
		Message:        fmt.Sprintf(errMsg, workflowID, runID),
		StartRequestID: createRequestID,
		RunID:          runID,
	}
}

func (e *historyEngineImpl) GetReplicationMessages(
	ctx context.Context,
	pollingCluster string,
	lastReadMessageID int64,
) (*types.ReplicationMessages, error) {

	scope := metrics.HistoryGetReplicationMessagesScope
	sw := e.metricsClient.StartTimer(scope, metrics.GetReplicationMessagesForShardLatency)
	defer sw.Stop()

	replicationMessages, err := e.replicationAckManager.GetTasks(
		ctx,
		pollingCluster,
		lastReadMessageID,
	)
	if err != nil {
		e.logger.Error("Failed to retrieve replication messages.", tag.Error(err))
		return nil, err
	}

	//Set cluster status for sync shard info
	replicationMessages.SyncShardStatus = &types.SyncShardStatus{
		Timestamp: common.Int64Ptr(e.timeSource.Now().UnixNano()),
	}
	e.logger.Debug("Successfully fetched replication messages.", tag.Counter(len(replicationMessages.ReplicationTasks)))
	return replicationMessages, nil
}

func (e *historyEngineImpl) GetDLQReplicationMessages(
	ctx context.Context,
	taskInfos []*types.ReplicationTaskInfo,
) ([]*types.ReplicationTask, error) {

	scope := metrics.HistoryGetDLQReplicationMessagesScope
	sw := e.metricsClient.StartTimer(scope, metrics.GetDLQReplicationMessagesLatency)
	defer sw.Stop()

	tasks := make([]*types.ReplicationTask, 0, len(taskInfos))
	for _, taskInfo := range taskInfos {
		task, err := e.replicationAckManager.GetTask(ctx, taskInfo)
		if err != nil {
			e.logger.Error("Failed to fetch DLQ replication messages.", tag.Error(err))
			return nil, err
		}
		if task != nil {
			tasks = append(tasks, task)
		}
	}

	return tasks, nil
}

func (e *historyEngineImpl) ReapplyEvents(
	ctx context.Context,
	domainUUID string,
	workflowID string,
	runID string,
	reapplyEvents []*types.HistoryEvent,
) error {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(domainUUID)
	if err != nil {
		switch {
		case domainEntry != nil && domainEntry.IsDomainPendingActive():
			return nil
		default:
			return err
		}
	}
	domainID := domainEntry.GetInfo().ID
	// remove run id from the execution so that reapply events to the current run
	currentExecution := types.WorkflowExecution{
		WorkflowID: workflowID,
	}

	return workflow.UpdateWithActionFunc(
		ctx,
		e.executionCache,
		domainID,
		currentExecution,
		e.timeSource.Now(),
		func(wfContext execution.Context, mutableState execution.MutableState) (*workflow.UpdateAction, error) {
			// Filter out reapply event from the same cluster
			toReapplyEvents := make([]*types.HistoryEvent, 0, len(reapplyEvents))
			lastWriteVersion, err := mutableState.GetLastWriteVersion()
			if err != nil {
				return nil, err
			}
			for _, event := range reapplyEvents {
				if event.GetVersion() == lastWriteVersion {
					// The reapply is from the same cluster. Ignoring.
					continue
				}
				dedupResource := definition.NewEventReappliedID(runID, event.GetEventID(), event.GetVersion())
				if mutableState.IsResourceDuplicated(dedupResource) {
					// already apply the signal
					continue
				}
				toReapplyEvents = append(toReapplyEvents, event)
			}
			if len(toReapplyEvents) == 0 {
				return &workflow.UpdateAction{
					Noop: true,
				}, nil
			}

			if !mutableState.IsWorkflowExecutionRunning() {
				// need to reset target workflow (which is also the current workflow)
				// to accept events to be reapplied
				baseRunID := mutableState.GetExecutionInfo().RunID
				resetRunID := uuid.New()
				baseRebuildLastEventID := mutableState.GetPreviousStartedEventID()

				// TODO when https://github.com/uber/cadence/issues/2420 is finished, remove this block,
				//  since cannot reapply event to a finished workflow which had no decisions started
				if baseRebuildLastEventID == common.EmptyEventID {
					e.logger.Warn("cannot reapply event to a finished workflow",
						tag.WorkflowDomainID(domainID),
						tag.WorkflowID(currentExecution.GetWorkflowID()),
					)
					e.metricsClient.IncCounter(metrics.HistoryReapplyEventsScope, metrics.EventReapplySkippedCount)
					return &workflow.UpdateAction{Noop: true}, nil
				}

				baseVersionHistories := mutableState.GetVersionHistories()
				if baseVersionHistories == nil {
					return nil, execution.ErrMissingVersionHistories
				}
				baseCurrentVersionHistory, err := baseVersionHistories.GetCurrentVersionHistory()
				if err != nil {
					return nil, err
				}
				baseRebuildLastEventVersion, err := baseCurrentVersionHistory.GetEventVersion(baseRebuildLastEventID)
				if err != nil {
					return nil, err
				}
				baseCurrentBranchToken := baseCurrentVersionHistory.GetBranchToken()
				baseNextEventID := mutableState.GetNextEventID()

				if err = e.workflowResetter.ResetWorkflow(
					ctx,
					domainID,
					workflowID,
					baseRunID,
					baseCurrentBranchToken,
					baseRebuildLastEventID,
					baseRebuildLastEventVersion,
					baseNextEventID,
					resetRunID,
					uuid.New(),
					execution.NewWorkflow(
						ctx,
						e.shard.GetDomainCache(),
						e.shard.GetClusterMetadata(),
						wfContext,
						mutableState,
						execution.NoopReleaseFn,
					),
					ndc.EventsReapplicationResetWorkflowReason,
					toReapplyEvents,
					false,
				); err != nil {
					return nil, err
				}
				return &workflow.UpdateAction{
					Noop: true,
				}, nil
			}

			postActions := &workflow.UpdateAction{
				CreateDecision: true,
			}
			// Do not create decision task when the workflow is cron and the cron has not been started yet
			if mutableState.GetExecutionInfo().CronSchedule != "" && !mutableState.HasProcessedOrPendingDecision() {
				postActions.CreateDecision = false
			}
			reappliedEvents, err := e.eventsReapplier.ReapplyEvents(
				ctx,
				mutableState,
				toReapplyEvents,
				runID,
			)
			if err != nil {
				e.logger.Error("failed to re-apply stale events", tag.Error(err))
				return nil, &types.InternalServiceError{Message: "unable to re-apply stale events"}
			}
			if len(reappliedEvents) == 0 {
				return &workflow.UpdateAction{
					Noop: true,
				}, nil
			}
			return postActions, nil
		},
	)
}

func (e *historyEngineImpl) ReadDLQMessages(
	ctx context.Context,
	request *types.ReadDLQMessagesRequest,
) (*types.ReadDLQMessagesResponse, error) {

	tasks, taskInfo, token, err := e.replicationDLQHandler.ReadMessages(
		ctx,
		request.GetSourceCluster(),
		request.GetInclusiveEndMessageID(),
		int(request.GetMaximumPageSize()),
		request.GetNextPageToken(),
	)
	if err != nil {
		return nil, err
	}
	return &types.ReadDLQMessagesResponse{
		Type:                 request.GetType().Ptr(),
		ReplicationTasks:     tasks,
		ReplicationTasksInfo: taskInfo,
		NextPageToken:        token,
	}, nil
}

func (e *historyEngineImpl) PurgeDLQMessages(
	ctx context.Context,
	request *types.PurgeDLQMessagesRequest,
) error {

	return e.replicationDLQHandler.PurgeMessages(
		ctx,
		request.GetSourceCluster(),
		request.GetInclusiveEndMessageID(),
	)
}

func (e *historyEngineImpl) MergeDLQMessages(
	ctx context.Context,
	request *types.MergeDLQMessagesRequest,
) (*types.MergeDLQMessagesResponse, error) {

	token, err := e.replicationDLQHandler.MergeMessages(
		ctx,
		request.GetSourceCluster(),
		request.GetInclusiveEndMessageID(),
		int(request.GetMaximumPageSize()),
		request.GetNextPageToken(),
	)
	if err != nil {
		return nil, err
	}
	return &types.MergeDLQMessagesResponse{
		NextPageToken: token,
	}, nil
}

func (e *historyEngineImpl) RefreshWorkflowTasks(
	ctx context.Context,
	domainUUID string,
	workflowExecution types.WorkflowExecution,
) (retError error) {

	domainEntry, err := e.shard.GetDomainCache().GetActiveDomainByID(domainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID

	wfContext, release, err := e.executionCache.GetOrCreateWorkflowExecution(ctx, domainID, workflowExecution)
	if err != nil {
		return err
	}
	defer func() { release(retError) }()

	mutableState, err := wfContext.LoadWorkflowExecution(ctx)
	if err != nil {
		return err
	}

	if !mutableState.IsWorkflowExecutionRunning() {
		return nil
	}

	mutableStateTaskRefresher := execution.NewMutableStateTaskRefresher(
		e.shard.GetConfig(),
		e.shard.GetDomainCache(),
		e.shard.GetEventsCache(),
		e.shard.GetLogger(),
		e.shard.GetShardID(),
	)

	now := e.shard.GetTimeSource().Now()

	err = mutableStateTaskRefresher.RefreshTasks(ctx, now, mutableState)
	if err != nil {
		return err
	}

	err = wfContext.UpdateWorkflowExecutionAsActive(ctx, now)
	if err != nil {
		return err
	}
	return nil
}

func (e *historyEngineImpl) newChildContext(
	parentCtx context.Context,
) (context.Context, context.CancelFunc) {

	ctxTimeout := contextLockTimeout
	if deadline, ok := parentCtx.Deadline(); ok {
		now := e.shard.GetTimeSource().Now()
		parentTimeout := deadline.Sub(now)
		if parentTimeout > 0 && parentTimeout < contextLockTimeout {
			ctxTimeout = parentTimeout
		}
	}
	return context.WithTimeout(context.Background(), ctxTimeout)
}
