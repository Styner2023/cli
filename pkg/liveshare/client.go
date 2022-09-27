// Package liveshare is a Go client library for the Visual Studio Live Share
// service, which provides collaborative, distributed editing and debugging.
// See https://docs.microsoft.com/en-us/visualstudio/liveshare for an overview.
//
// It provides the ability for a Go program to connect to a Live Share
// workspace (Connect), to expose a TCP port on a remote host
// (UpdateSharedVisibility), to start an SSH server listening on an
// exposed port (StartSSHServer), and to forward connections between
// the remote port and a local listening TCP port (ForwardToListener)
// or a local Go reader/writer (Forward).
package liveshare

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/cli/cli/v2/internal/codespaces/grpc"
	"github.com/opentracing/opentracing-go"
)

const (
	codespacesInternalPort        = 16634
	codespacesInternalSessionName = "CodespacesInternal"
)

type logger interface {
	Println(v ...interface{})
	Printf(f string, v ...interface{})
}

// An Options specifies Live Share connection parameters.
type Options struct {
	ClientName     string // ClientName is the name of the connecting client.
	SessionID      string
	SessionToken   string // token for SSH session
	RelaySAS       string
	RelayEndpoint  string
	HostPublicKeys []string
	Logger         logger      // required
	TLSConfig      *tls.Config // (optional)
}

// uri returns a websocket URL for the specified options.
func (opts *Options) uri(action string) (string, error) {
	if opts.ClientName == "" {
		return "", errors.New("ClientName is required")
	}
	if opts.SessionID == "" {
		return "", errors.New("SessionID is required")
	}
	if opts.RelaySAS == "" {
		return "", errors.New("RelaySAS is required")
	}
	if opts.RelayEndpoint == "" {
		return "", errors.New("RelayEndpoint is required")
	}

	sas := url.QueryEscape(opts.RelaySAS)
	uri := opts.RelayEndpoint

	if strings.HasPrefix(uri, "http:") {
		uri = strings.Replace(uri, "http:", "ws:", 1)
	} else {
		uri = strings.Replace(uri, "sb:", "wss:", -1)
	}

	uri = strings.Replace(uri, ".net/", ".net:443/$hc/", 1)
	uri = uri + "?sb-hc-action=" + action + "&sb-hc-token=" + sas
	return uri, nil
}

// Connect connects to a Live Share workspace specified by the
// options, and returns a session representing the connection.
// The caller must call the session's Close method to end the session.
func Connect(ctx context.Context, opts Options) (*Session, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "Connect")
	defer span.Finish()

	uri, err := opts.uri("connect")
	if err != nil {
		return nil, err
	}

	sock := newSocket(uri, opts.TLSConfig)
	if err := sock.connect(ctx); err != nil {
		return nil, fmt.Errorf("error connecting websocket: %w", err)
	}

	if opts.SessionToken == "" {
		return nil, errors.New("SessionToken is required")
	}
	ssh := newSSHSession(opts.SessionToken, opts.HostPublicKeys, sock)
	if err := ssh.connect(ctx); err != nil {
		return nil, fmt.Errorf("error connecting to ssh session: %w", err)
	}

	rpc := newRPCClient(ssh)
	rpc.connect(ctx)

	args := joinWorkspaceArgs{
		ID:                      opts.SessionID,
		ConnectionMode:          "local",
		JoiningUserSessionToken: opts.SessionToken,
		ClientCapabilities: clientCapabilities{
			IsNonInteractive: false,
		},
	}
	var result joinWorkspaceResult
	if err := rpc.do(ctx, "workspace.joinWorkspace", &args, &result); err != nil {
		return nil, fmt.Errorf("error joining Live Share workspace: %w", err)
	}

	s := &Session{
		ssh:             ssh,
		rpc:             rpc,
		grpc:            grpc.NewClient(),
		clientName:      opts.ClientName,
		keepAliveReason: make(chan string, 1),
		logger:          opts.Logger,
	}
	go s.heartbeat(ctx, 1*time.Minute)

	// Connect to the gRPC server so we can make requests anywhere we have access to the session
	err = s.connectToGrpcServer(ctx, opts.SessionToken)
	if err != nil {
		return nil, fmt.Errorf("error connecting to internal server: %w", err)
	}

	return s, nil
}

// Connects to the gRPC server running on the host VM
func (s *Session) connectToGrpcServer(ctx context.Context, token string) error {
	listen, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", 0))
	if err != nil {
		return fmt.Errorf("failed to listen to local port over tcp: %w", err)
	}

	// Tunnel the remote gRPC server port to the local port
	localGrpcServerPort := listen.Addr().(*net.TCPAddr).Port
	internalTunnelClosed := make(chan error, 1)
	go func() {
		fwd := NewPortForwarder(s, codespacesInternalSessionName, codespacesInternalPort, true)
		internalTunnelClosed <- fwd.ForwardToListener(ctx, listen)
	}()

	// Make a connection to the gRPC server
	err = s.grpc.Connect(ctx, listen, localGrpcServerPort, token)

	if err != nil {
		return fmt.Errorf("failed to establish connection on port %d: %w", localGrpcServerPort, err)
	}

	select {
	case err := <-internalTunnelClosed:
		return fmt.Errorf("internal tunnel closed: %w", err)
	default:
		return nil // success
	}
}

type clientCapabilities struct {
	IsNonInteractive bool `json:"isNonInteractive"`
}

type joinWorkspaceArgs struct {
	ID                      string             `json:"id"`
	ConnectionMode          string             `json:"connectionMode"`
	JoiningUserSessionToken string             `json:"joiningUserSessionToken"`
	ClientCapabilities      clientCapabilities `json:"clientCapabilities"`
}

type joinWorkspaceResult struct {
	SessionNumber int `json:"sessionNumber"`
}
