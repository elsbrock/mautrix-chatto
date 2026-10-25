package chattoclient

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/bridgemetrics"
	realtimev1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/realtime/v1"
)

// RealtimeProtocolVersion is the realtime behavioural protocol version this
// client implements.
const RealtimeProtocolVersion = 4

const realtimePath = "/api/realtime"

// RealtimeHandler receives realtime frames. Methods are called sequentially
// from the read loop; a slow handler delays later events, which is intended:
// cursors only advance after their event was handled.
type RealtimeHandler interface {
	// HandleEvent processes one authorized event.
	HandleEvent(ctx context.Context, evt *realtimev1.RealtimeEvent)
	// HandleSnapshot receives the server's bounded state snapshot, sent when
	// a subscription could not resume.
	HandleSnapshot(ctx context.Context, snapshot *realtimev1.RealtimeSnapshot)
	// HandleCaughtUp is called when the stream reaches live delivery. Any
	// recovery other than RESUMED means events may have been missed and the
	// handler should forward-fill from the timeline APIs.
	HandleCaughtUp(ctx context.Context, recovery realtimev1.RealtimeRecovery)
	// HandleState reports connection state changes.
	HandleState(state RealtimeState, err error)
	// SaveCursor persists the latest safe resume cursor.
	SaveCursor(cursor string)
}

// RealtimeState is the connection state of a realtime subscription.
type RealtimeState int

const (
	RealtimeConnecting RealtimeState = iota
	RealtimeConnected
	RealtimeTransientDisconnect
	RealtimeBadCredentials
	RealtimeStopped
)

func (s RealtimeState) String() string {
	switch s {
	case RealtimeConnecting:
		return "connecting"
	case RealtimeConnected:
		return "connected"
	case RealtimeTransientDisconnect:
		return "transient_disconnect"
	case RealtimeBadCredentials:
		return "bad_credentials"
	case RealtimeStopped:
		return "stopped"
	}
	return "unknown"
}

// ErrRealtimeFatal wraps close reasons after which reconnecting is pointless.
var ErrRealtimeFatal = errors.New("chatto realtime: fatal close")

// CloseError describes a server-requested close.
type CloseError struct {
	Code       realtimev1.RealtimeCloseCode
	Message    string
	Reconnect  bool
	RetryAfter time.Duration
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("chatto realtime closed: %s: %s", strings.TrimPrefix(e.Code.String(), "REALTIME_CLOSE_CODE_"), e.Message)
}

// Realtime maintains one realtime subscription with reconnects.
type Realtime struct {
	client  *Client
	handler RealtimeHandler
	log     zerolog.Logger
	label   string

	// MinBackoff and MaxBackoff bound the reconnect delay.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// HeartbeatTimeout is how long the socket may stay silent before it is
	// considered dead. Chatto's own client uses 75 s.
	HeartbeatTimeout time.Duration
	// ReadLimit caps the size of one frame; snapshots can be large.
	ReadLimit int64
	// Dial overrides how the socket is opened, for tests.
	Dial func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)

	mu        sync.Mutex
	cursor    string
	lastToken string
}

// NewRealtime creates a subscription manager. cursor may be a previously
// saved resume cursor; label identifies the login in metrics.
func NewRealtime(client *Client, handler RealtimeHandler, cursor, label string, log zerolog.Logger) *Realtime {
	return &Realtime{
		client:           client,
		handler:          handler,
		log:              log,
		label:            label,
		cursor:           cursor,
		MinBackoff:       time.Second,
		MaxBackoff:       2 * time.Minute,
		HeartbeatTimeout: 75 * time.Second,
		ReadLimit:        64 << 20,
		Dial:             websocket.Dial,
	}
}

// Cursor returns the latest safe resume cursor.
func (r *Realtime) Cursor() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

func (r *Realtime) setCursor(cursor string) {
	if cursor == "" {
		return
	}
	r.mu.Lock()
	changed := r.cursor != cursor
	r.cursor = cursor
	r.mu.Unlock()
	if changed {
		r.handler.SaveCursor(cursor)
	}
}

func (r *Realtime) clearCursor() {
	r.mu.Lock()
	r.cursor = ""
	r.mu.Unlock()
}

// URL returns the realtime WebSocket URL of the client's server.
func (r *Realtime) URL() string {
	u := r.client.BaseURL()
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = realtimePath
	return u.String()
}

// Run connects and reconnects until ctx is cancelled or a fatal close is
// received. It always returns a non-nil error.
func (r *Realtime) Run(ctx context.Context) error {
	backoff := r.MinBackoff
	for {
		r.handler.HandleState(RealtimeConnecting, nil)
		connected, err := r.runOnce(ctx)
		bridgemetrics.RealtimeConnected.WithLabelValues(r.label).Set(0)
		if ctx.Err() != nil {
			r.handler.HandleState(RealtimeStopped, nil)
			return ctx.Err()
		}
		if connected {
			backoff = r.MinBackoff
		}
		delay := backoff
		reason := "error"
		var closeErr *CloseError
		switch {
		case errors.As(err, &closeErr):
			reason = strings.ToLower(strings.TrimPrefix(closeErr.Code.String(), "REALTIME_CLOSE_CODE_"))
			fatal, retry := r.classifyClose(closeErr)
			if fatal {
				r.handler.HandleState(RealtimeBadCredentials, err)
				return fmt.Errorf("%w: %w", ErrRealtimeFatal, err)
			}
			if retry > delay {
				delay = retry
			}
		case errors.Is(err, ErrSessionRevoked), errors.Is(err, errBadCredentials):
			r.handler.HandleState(RealtimeBadCredentials, err)
			return fmt.Errorf("%w: %w", ErrRealtimeFatal, err)
		case errors.Is(err, errHeartbeatTimeout):
			reason = "heartbeat_timeout"
		}
		bridgemetrics.RealtimeReconnects.WithLabelValues(reason).Inc()
		r.handler.HandleState(RealtimeTransientDisconnect, err)
		// Full jitter in [delay/2, delay] avoids reconnect stampedes.
		sleep := delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
		r.log.Warn().Err(err).Dur("retry_in", sleep).Msg("Realtime connection lost")
		select {
		case <-ctx.Done():
			r.handler.HandleState(RealtimeStopped, nil)
			return ctx.Err()
		case <-time.After(sleep):
		}
		backoff = min(backoff*2, r.MaxBackoff)
	}
}

var (
	errHeartbeatTimeout = errors.New("chatto realtime: heartbeat timeout")
	errBadCredentials   = errors.New("chatto realtime: credentials rejected")
)

// classifyClose decides whether a close is fatal and how long to wait.
func (r *Realtime) classifyClose(c *CloseError) (fatal bool, retry time.Duration) {
	switch c.Code {
	case realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_SESSION_TERMINATED,
		realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_UNSUPPORTED_PROTOCOL,
		realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_INVALID_REQUEST:
		return true, 0
	case realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_AUTHENTICATION_REQUIRED,
		realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_SESSION_RENEWAL_REQUIRED:
		if _, static := r.client.tokens.(StaticToken); static && !c.Reconnect {
			return true, 0
		}
		// Expired access token: refresh and come back quickly.
		r.client.tokens.Invalidate(r.lastToken)
		return false, c.RetryAfter
	case realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_RESYNC_REQUIRED:
		r.clearCursor()
		return false, c.RetryAfter
	default:
		return false, c.RetryAfter
	}
}

func (r *Realtime) runOnce(ctx context.Context) (connected bool, err error) {
	if r.client.tokens == nil {
		return false, errBadCredentials
	}
	token, err := r.client.tokens.Token(ctx)
	if err != nil {
		return false, err
	}
	r.lastToken = token
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(connCtx, 30*time.Second)
	conn, resp, err := r.Dial(dialCtx, r.URL(), &websocket.DialOptions{
		HTTPClient: r.client.http,
		HTTPHeader: http.Header{"User-Agent": []string{r.client.userAgent}},
	})
	dialCancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("chatto realtime: dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(r.ReadLimit)

	cursor := r.Cursor()
	sub := &realtimev1.RealtimeSubscribe{
		ProtocolVersion: RealtimeProtocolVersion,
		BearerToken:     &token,
		InitialState:    realtimev1.RealtimeInitialState_REALTIME_INITIAL_STATE_SNAPSHOT,
	}
	if cursor != "" {
		sub.ResumeCursor = &cursor
	}
	data, err := proto.Marshal(sub)
	if err != nil {
		return false, err
	}
	if err := conn.Write(connCtx, websocket.MessageBinary, data); err != nil {
		return false, fmt.Errorf("chatto realtime: subscribe: %w", err)
	}

	// The watchdog cancels the read when the socket stays silent too long.
	activity := make(chan struct{}, 1)
	timedOut := make(chan struct{})
	go func() {
		timer := time.NewTimer(r.HeartbeatTimeout)
		defer timer.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(r.HeartbeatTimeout)
			case <-timer.C:
				close(timedOut)
				cancel()
				return
			}
		}
	}()

	var snapshot *realtimev1.RealtimeSnapshot
	for {
		typ, payload, err := conn.Read(connCtx)
		if err != nil {
			select {
			case <-timedOut:
				return connected, errHeartbeatTimeout
			default:
			}
			if ctx.Err() != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "shutting down")
				return connected, ctx.Err()
			}
			return connected, fmt.Errorf("chatto realtime: read: %w", err)
		}
		select {
		case activity <- struct{}{}:
		default:
		}
		if typ != websocket.MessageBinary {
			continue
		}
		var frame realtimev1.RealtimeServerFrame
		if err := proto.Unmarshal(payload, &frame); err != nil {
			r.log.Warn().Err(err).Msg("Dropping undecodable realtime frame")
			continue
		}
		switch f := frame.Frame.(type) {
		case *realtimev1.RealtimeServerFrame_Event:
			r.observeEvent(f.Event)
			r.handler.HandleEvent(ctx, f.Event)
			r.setCursor(f.Event.GetCursor())
		case *realtimev1.RealtimeServerFrame_Heartbeat:
			r.setCursor(f.Heartbeat.GetCursor())
		case *realtimev1.RealtimeServerFrame_Snapshot:
			// A snapshot only counts once the stream has caught up.
			snapshot = f.Snapshot
		case *realtimev1.RealtimeServerFrame_CaughtUp:
			connected = true
			bridgemetrics.RealtimeConnected.WithLabelValues(r.label).Set(1)
			result := strings.ToLower(strings.TrimPrefix(f.CaughtUp.GetRecovery().String(), "REALTIME_RECOVERY_"))
			bridgemetrics.RealtimeRecoveries.WithLabelValues(result).Inc()
			if snapshot != nil {
				r.handler.HandleSnapshot(ctx, snapshot)
				snapshot = nil
			}
			r.setCursor(f.CaughtUp.GetCursor())
			r.handler.HandleState(RealtimeConnected, nil)
			r.handler.HandleCaughtUp(ctx, f.CaughtUp.GetRecovery())
		case *realtimev1.RealtimeServerFrame_Close:
			closeErr := &CloseError{
				Code:      f.Close.GetCode(),
				Message:   f.Close.GetMessage(),
				Reconnect: f.Close.GetReconnect(),
			}
			if ra := f.Close.GetRetryAfter(); ra != nil {
				closeErr.RetryAfter = ra.AsDuration()
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return connected, closeErr
		}
	}
}

func (r *Realtime) observeEvent(evt *realtimev1.RealtimeEvent) {
	bridgemetrics.RealtimeEvents.WithLabelValues(EventType(evt)).Inc()
	if ts := evt.GetCreatedAt(); ts != nil {
		lag := time.Since(ts.AsTime())
		if lag >= 0 {
			bridgemetrics.RealtimeEventLag.Observe(lag.Seconds())
		}
	}
}

// EventType returns a short name for an event's payload, e.g. "message_posted".
func EventType(evt *realtimev1.RealtimeEvent) string {
	if evt.GetEvent() == nil {
		return "unknown"
	}
	fields := evt.ProtoReflect().WhichOneof(evt.ProtoReflect().Descriptor().Oneofs().ByName("event"))
	if fields == nil {
		return "unknown"
	}
	return string(fields.Name())
}
