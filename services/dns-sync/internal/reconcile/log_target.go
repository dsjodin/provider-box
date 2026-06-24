package reconcile

import (
	"context"
	"log/slog"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/model"
)

// LogTarget is the scaffold-time Target. It treats Technitium as an empty
// store (so every desired record becomes a create) and logs the ops instead of
// applying them. It is replaced by a real Technitium client once the API
// endpoint/token flow has been verified against a running container.
type LogTarget struct {
	Logger *slog.Logger
}

func (*LogTarget) List(ctx context.Context) ([]model.Record, []string, error) {
	return nil, nil, nil
}

func (l *LogTarget) Apply(ctx context.Context, ops []model.Op) error {
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for _, op := range ops {
		logger.Info("dry-run op",
			"kind", op.Kind.String(),
			"zone", op.Zone,
			"name", op.Record.Name,
			"type", op.Record.Type,
			"data", op.Record.Data,
		)
	}
	return nil
}
