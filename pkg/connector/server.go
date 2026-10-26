package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/rs/zerolog"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
)

// server is the runtime state of one configured Chatto server.
type server struct {
	ID  string
	URL string

	uploadLimit uploadLimit

	versionLock sync.Mutex
	version     string
	// tooOld is set when strict_server_version rejects the server.
	tooOld bool
}

func (s *server) serverVersion() (version string, tooOld bool) {
	s.versionLock.Lock()
	defer s.versionLock.Unlock()
	return s.version, s.tooOld
}

// server returns a configured server, or nil.
func (c *Connector) server(id string) *server {
	c.serversLock.Lock()
	defer c.serversLock.Unlock()
	if c.servers == nil {
		c.servers = make(map[string]*server, len(c.Config.Servers))
	}
	if srv, ok := c.servers[id]; ok {
		return srv
	}
	cfg := c.Config.Server(id)
	if cfg == nil {
		return nil
	}
	srv := &server{ID: cfg.ID, URL: cfg.URL}
	c.servers[id] = srv
	return srv
}

// serverIDs returns the configured server IDs in config order.
func (c *Connector) serverIDs() []string {
	ids := make([]string, len(c.Config.Servers))
	for i, srv := range c.Config.Servers {
		ids[i] = srv.ID
	}
	return ids
}

// checkServerVersion fetches the server version and records whether the
// server is supported. It returns an error only for transport failures.
func (c *Connector) checkServerVersion(ctx context.Context, srv *server) error {
	log := zerolog.Ctx(ctx).With().Str("server", srv.ID).Logger()
	client, err := c.newClient(srv, nil)
	if err != nil {
		return err
	}
	version, err := client.ServerVersion(ctx)
	if err != nil {
		return err
	}
	supported := CheckServerVersion(version)
	srv.versionLock.Lock()
	srv.version = version
	srv.tooOld = !supported && c.Config.StrictServerVersion
	srv.versionLock.Unlock()
	bridgemetrics.ServerInfo.WithLabelValues(srv.ID, version, fmt.Sprint(supported)).Set(1)
	if !supported {
		log.Warn().Str("server_version", version).Bool("refused", c.Config.StrictServerVersion).
			Msgf("Chatto server version %s is older than %s, which this bridge was built against", version, chattoclient.UpstreamVersion)
	} else {
		log.Info().Str("server_version", version).Msg("Connected to Chatto server")
	}
	return nil
}

var errServerMismatch = errors.New("this Chatto account is on a different server than the room")
