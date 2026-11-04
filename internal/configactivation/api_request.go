package configactivation

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

type apiOutcomeEncoderKey struct{}
type apiOutcomeEncoder func(Result, error) ([]byte, error)

// WithAPIOutcomeEncoder supplies only pure, bounded response encoding. The
// existing Owner controls reservation, execution, native outcome and storage.
// The encoder is never called under SQL locks or used as an activation port.
func WithAPIOutcomeEncoder(ctx context.Context, encode func(Result, error) ([]byte, error)) (context.Context, error) {
	if ctx == nil || encode == nil {
		return nil, ErrInvalidStartup
	}
	if _, present := ctx.Value(apiOutcomeEncoderKey{}).(apiOutcomeEncoder); present {
		return nil, ErrInvalidStartup
	}
	return context.WithValue(ctx, apiOutcomeEncoderKey{}, apiOutcomeEncoder(encode)), nil
}

type apiOutcomeBinding struct {
	execution storage.ConfigurationAPIExecution
	encode    apiOutcomeEncoder
}

func captureAPIOutcome(ctx context.Context) *apiOutcomeBinding {
	execution, present := storage.ConfigurationAPIExecutionFromContext(ctx)
	if !present {
		return nil
	}
	encode, present := ctx.Value(apiOutcomeEncoderKey{}).(apiOutcomeEncoder)
	if !present || encode == nil {
		return nil
	}
	return &apiOutcomeBinding{execution: execution, encode: encode}
}

func knownNoCommit(result Result) bool {
	if result.transaction == storage.TransactionNotCommitted {
		return true
	}
	return result.transaction == storage.TransactionOutcomeUnspecified && (result.outcome == RejectedBeforeCommit || result.outcome == NotCommitted || result.outcome == RestartRequired)
}

func (o *Owner) finalizeAPIOutcome(binding *apiOutcomeBinding, result Result, cause error) Result {
	if binding == nil || !binding.execution.Valid() {
		return result
	}
	result = withExecution(binding.execution, result)
	if result.outcome == NoChange || !knownNoCommit(result) {
		return result
	}
	data, err := binding.encode(result, cause)
	if err != nil || len(data) == 0 || len(data) > storage.ConfigurationAPIFailureBytes {
		return result
	}
	// A lost HTTP response cannot discard a known native rejection. This bounded
	// metadata append has no compiler, workspace stage, driver or authority call.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(o.ctx), 2*time.Second)
	defer cancel()
	_, _, _ = binding.execution.RecordFailure(ctx, o.input.Store, data)
	return result
}

func (o *Owner) finalizeAPIContext(ctx context.Context, result Result, cause error) Result {
	return o.finalizeAPIOutcome(captureAPIOutcome(ctx), result, cause)
}
