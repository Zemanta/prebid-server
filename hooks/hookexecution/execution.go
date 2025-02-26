package hookexecution

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prebid/prebid-server/hooks"
	"github.com/prebid/prebid-server/hooks/hookstage"
)

type hookResponse[T any] struct {
	Err           error
	ExecutionTime time.Duration
	HookID        HookID
	Result        hookstage.HookResult[T]
}

type hookHandler[H any, P any] func(
	context.Context,
	hookstage.ModuleInvocationContext,
	H,
	P,
) (hookstage.HookResult[P], error)

func executeStage[H any, P any](
	executionCtx executionContext,
	plan hooks.Plan[H],
	payload P,
	hookHandler hookHandler[H, P],
) (StageOutcome, P, stageModuleContext, *RejectError) {
	stageOutcome := StageOutcome{}
	stageOutcome.Groups = make([]GroupOutcome, 0, len(plan))
	stageModuleCtx := stageModuleContext{}
	stageModuleCtx.groupCtx = make([]groupModuleContext, 0, len(plan))

	for _, group := range plan {
		groupOutcome, newPayload, moduleContexts, rejectErr := executeGroup(executionCtx, group, payload, hookHandler)
		stageOutcome.ExecutionTimeMillis += groupOutcome.ExecutionTimeMillis
		stageOutcome.Groups = append(stageOutcome.Groups, groupOutcome)
		stageModuleCtx.groupCtx = append(stageModuleCtx.groupCtx, moduleContexts)
		if rejectErr != nil {
			return stageOutcome, payload, stageModuleCtx, rejectErr
		}

		payload = newPayload
	}

	return stageOutcome, payload, stageModuleCtx, nil
}

func executeGroup[H any, P any](
	executionCtx executionContext,
	group hooks.Group[H],
	payload P,
	hookHandler hookHandler[H, P],
) (GroupOutcome, P, groupModuleContext, *RejectError) {
	var wg sync.WaitGroup
	rejected := make(chan struct{})
	resp := make(chan hookResponse[P])

	for _, hook := range group.Hooks {
		mCtx := executionCtx.getModuleContext(hook.Module)
		wg.Add(1)
		go func(hw hooks.HookWrapper[H], moduleCtx hookstage.ModuleInvocationContext) {
			defer wg.Done()
			executeHook(moduleCtx, hw, payload, hookHandler, group.Timeout, resp, rejected)
		}(hook, mCtx)
	}

	go func() {
		wg.Wait()
		close(resp)
	}()

	hookResponses := collectHookResponses(resp, rejected)

	return handleHookResponses(executionCtx, hookResponses, payload)
}

func executeHook[H any, P any](
	moduleCtx hookstage.ModuleInvocationContext,
	hw hooks.HookWrapper[H],
	payload P,
	hookHandler hookHandler[H, P],
	timeout time.Duration,
	resp chan<- hookResponse[P],
	rejected <-chan struct{},
) {
	hookRespCh := make(chan hookResponse[P], 1)
	startTime := time.Now()
	hookId := HookID{ModuleCode: hw.Module, HookImplCode: hw.Code}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		result, err := hookHandler(ctx, moduleCtx, hw.Hook, payload)
		hookRespCh <- hookResponse[P]{
			Result: result,
			Err:    err,
		}
	}()

	select {
	case res := <-hookRespCh:
		res.HookID = hookId
		res.ExecutionTime = time.Since(startTime)
		resp <- res
	case <-time.After(timeout):
		resp <- hookResponse[P]{
			Err:           TimeoutError{},
			ExecutionTime: time.Since(startTime),
			HookID:        hookId,
			Result:        hookstage.HookResult[P]{},
		}
	case <-rejected:
		return
	}
}

func collectHookResponses[P any](resp <-chan hookResponse[P], rejected chan<- struct{}) []hookResponse[P] {
	hookResponses := make([]hookResponse[P], 0)
	for r := range resp {
		hookResponses = append(hookResponses, r)
		if r.Result.Reject {
			close(rejected)
			break
		}
	}

	return hookResponses
}

func handleHookResponses[P any](
	executionCtx executionContext,
	hookResponses []hookResponse[P],
	payload P,
) (GroupOutcome, P, groupModuleContext, *RejectError) {
	groupOutcome := GroupOutcome{}
	groupOutcome.InvocationResults = make([]HookOutcome, 0, len(hookResponses))
	groupModuleCtx := make(groupModuleContext, len(hookResponses))

	for _, r := range hookResponses {
		groupModuleCtx[r.HookID.ModuleCode] = r.Result.ModuleContext
		if r.ExecutionTime > groupOutcome.ExecutionTimeMillis {
			groupOutcome.ExecutionTimeMillis = r.ExecutionTime
		}

		updatedPayload, hookOutcome, rejectErr := handleHookResponse(executionCtx, payload, r)
		groupOutcome.InvocationResults = append(groupOutcome.InvocationResults, hookOutcome)
		payload = updatedPayload

		if rejectErr != nil {
			return groupOutcome, payload, groupModuleCtx, rejectErr
		}
	}

	return groupOutcome, payload, groupModuleCtx, nil
}

// handleHookResponse is a strategy function that selects and applies
// one of the available algorithms to handle hook response.
func handleHookResponse[P any](
	ctx executionContext,
	payload P,
	hr hookResponse[P],
) (P, HookOutcome, *RejectError) {
	var rejectErr *RejectError
	hookOutcome := HookOutcome{
		Status:        StatusSuccess,
		HookID:        hr.HookID,
		Message:       hr.Result.Message,
		Errors:        hr.Result.Errors,
		Warnings:      hr.Result.Warnings,
		DebugMessages: hr.Result.DebugMessages,
		AnalyticsTags: hr.Result.AnalyticsTags,
		ExecutionTime: ExecutionTime{ExecutionTimeMillis: hr.ExecutionTime},
	}

	switch true {
	case hr.Err != nil:
		handleHookError(hr, &hookOutcome)
	case hr.Result.Reject:
		rejectErr = handleHookReject(ctx, hr, &hookOutcome)
	default:
		payload = handleHookMutations(payload, hr, &hookOutcome)
	}

	return payload, hookOutcome, rejectErr
}

// handleHookError sets an appropriate status to HookOutcome depending on the type of hook execution error.
func handleHookError[P any](hr hookResponse[P], hookOutcome *HookOutcome) {
	if hr.Err == nil {
		return
	}

	hookOutcome.Errors = append(hookOutcome.Errors, hr.Err.Error())
	switch hr.Err.(type) {
	case TimeoutError:
		hookOutcome.Status = StatusTimeout
	case FailureError:
		hookOutcome.Status = StatusFailure
	default:
		hookOutcome.Status = StatusExecutionFailure
	}
}

// handleHookReject rejects execution at the current stage.
// In case the stage does not support rejection, hook execution marked as failed.
func handleHookReject[P any](ctx executionContext, hr hookResponse[P], hookOutcome *HookOutcome) *RejectError {
	if !hr.Result.Reject {
		return nil
	}

	stage := hooks.Stage(ctx.stage)
	if !stage.IsRejectable() {
		hookOutcome.Status = StatusExecutionFailure
		hookOutcome.Errors = append(
			hookOutcome.Errors,
			fmt.Sprintf(
				"Module (name: %s, hook code: %s) tried to reject request on the %s stage that does not support rejection",
				hr.HookID.ModuleCode,
				hr.HookID.HookImplCode,
				ctx.stage,
			),
		)
		return nil
	}

	rejectErr := &RejectError{NBR: hr.Result.NbrCode, Hook: hr.HookID, Stage: ctx.stage}
	hookOutcome.Action = ActionReject
	hookOutcome.Errors = append(hookOutcome.Errors, rejectErr.Error())

	return rejectErr
}

// handleHookMutations applies mutations returned by hook to provided payload.
func handleHookMutations[P any](payload P, hr hookResponse[P], hookOutcome *HookOutcome) P {
	if hr.Result.ChangeSet == nil || len(hr.Result.ChangeSet.Mutations()) == 0 {
		hookOutcome.Action = ActionNone
		return payload
	}

	hookOutcome.Action = ActionUpdate
	for _, mut := range hr.Result.ChangeSet.Mutations() {
		p, err := mut.Apply(payload)
		if err != nil {
			hookOutcome.Warnings = append(
				hookOutcome.Warnings,
				fmt.Sprintf("failed to apply hook mutation: %s", err),
			)
			continue
		}

		payload = p
		hookOutcome.DebugMessages = append(
			hookOutcome.DebugMessages,
			fmt.Sprintf(
				"Hook mutation successfully applied, affected key: %s, mutation type: %s",
				strings.Join(mut.Key(), "."),
				mut.Type(),
			),
		)
	}

	return payload
}
