package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bytedance/sonic"
	"github.com/nrmnqdds/gomaluum/internal/dtos"
	gei "github.com/nrmnqdds/gomaluum/internal/proto/gei"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// geiClient is gomaluum's client for GEI, its shared encrypted store: schedules
// (scrape cache) and sessions (persistent i-Ma'luum cookie cache). Both share
// one gRPC connection. Persisting in GEI survives restarts and instances that an
// in-process cache cannot span.
type geiClient struct {
	conn     *grpc.ClientConn
	schedule gei.ScheduleIndexerClient
	session  gei.SessionIndexerClient
	adminKey string
}

// newGEIClient dials GEI and returns a client. adminKey guards writes.
func newGEIClient(serviceURL, adminKey string) (*geiClient, error) {
	conn, err := grpc.NewClient(serviceURL,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to GEI at %s: %w", serviceURL, err)
	}
	return &geiClient{
		conn:     conn,
		schedule: gei.NewScheduleIndexerClient(conn),
		session:  gei.NewSessionIndexerClient(conn),
		adminKey: adminKey,
	}, nil
}

func (i *geiClient) Close() error {
	if i.conn != nil {
		return i.conn.Close()
	}
	return nil
}

// GetSchedule returns the cached schedule for username. found is false (with a
// nil error) when the user has nothing cached yet.
func (i *geiClient) GetSchedule(ctx context.Context, username string) (schedules []dtos.ScheduleResponse, found bool, err error) {
	resp, err := i.schedule.GetSchedule(ctx, &gei.GetScheduleRequest{Username: username})
	if err != nil {
		return nil, false, err
	}
	if !resp.GetSuccess() {
		slog.DebugContext(ctx, "GEI cache miss", "username", username)
		return nil, false, nil
	}
	if err := sonic.ConfigFastest.Unmarshal([]byte(resp.GetScheduleJson()), &schedules); err != nil {
		return nil, false, fmt.Errorf("decoding cached schedule: %w", err)
	}
	slog.DebugContext(ctx, "GEI cache hit", "username", username, "sessions", len(schedules))
	return schedules, true, nil
}

// StoreSchedule caches the schedule for username. The admin key is attached as
// request metadata because GEI guards writes.
func (i *geiClient) StoreSchedule(ctx context.Context, username string, schedules []dtos.ScheduleResponse) error {
	payload, err := sonic.ConfigFastest.Marshal(schedules)
	if err != nil {
		return fmt.Errorf("encoding schedule for cache: %w", err)
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "admin-key", i.adminKey)
	if _, err = i.schedule.StoreSchedule(ctx, &gei.StoreScheduleRequest{
		Username:     username,
		ScheduleJson: string(payload),
	}); err != nil {
		return err
	}
	slog.DebugContext(ctx, "stored schedule to GEI", "username", username, "sessions", len(schedules), "bytes", len(payload))
	return nil
}

// GetSession returns the persisted i-Ma'luum cookie for username. found is false
// (with a nil error) when there is no stored session.
func (i *geiClient) GetSession(ctx context.Context, username string) (cookie string, found bool, err error) {
	resp, err := i.session.GetSession(ctx, &gei.GetSessionRequest{Username: username})
	if err != nil {
		return "", false, err
	}
	if !resp.GetSuccess() {
		slog.DebugContext(ctx, "GEI session miss", "username", username)
		return "", false, nil
	}
	slog.DebugContext(ctx, "GEI session hit", "username", username)
	return resp.GetCookie(), true, nil
}

// StoreSession persists the i-Ma'luum cookie for username. Admin-key guarded.
func (i *geiClient) StoreSession(ctx context.Context, username, cookie string) error {
	ctx = metadata.AppendToOutgoingContext(ctx, "admin-key", i.adminKey)
	if _, err := i.session.StoreSession(ctx, &gei.StoreSessionRequest{
		Username: username,
		Cookie:   cookie,
	}); err != nil {
		return err
	}
	slog.DebugContext(ctx, "stored session to GEI", "username", username)
	return nil
}

// DeleteSession removes the persisted session for username (on stale). Admin-key
// guarded.
func (i *geiClient) DeleteSession(ctx context.Context, username string) error {
	ctx = metadata.AppendToOutgoingContext(ctx, "admin-key", i.adminKey)
	if _, err := i.session.DeleteSession(ctx, &gei.DeleteSessionRequest{Username: username}); err != nil {
		return err
	}
	slog.DebugContext(ctx, "deleted session from GEI", "username", username)
	return nil
}
