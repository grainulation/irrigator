package managed

import (
	"context"

	"github.com/grainulation/irrigator/internal/model"
)

func managedWork(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding) (model.WorkResponse, error) {
	return ManagedWorkWithHistory(ctx, cell, prefix, req, b, nil)
}
