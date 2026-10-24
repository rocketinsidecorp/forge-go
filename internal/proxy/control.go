package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	maxControlRequest  = 64 << 10
	controlReadTimeout = 10 * time.Second
)

// ControlRequest is one message on the control socket.
type ControlRequest struct {
	// Op is deploy, remove, maintenance or status.
	Op           string         `json:"op"`
	Deploy       *DeployRequest `json:"deploy,omitempty"`
	Service      string         `json:"service,omitempty"`
	Maintenance  bool           `json:"maintenance,omitempty"`
	DrainTimeout Duration       `json:"drain_timeout,omitempty"`
}

// ControlResponse answers a [ControlRequest].
type ControlResponse struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Code     string          `json:"code,omitempty"`
	Deploy   *DeployResult   `json:"deploy,omitempty"`
	Services []ServiceStatus `json:"services,omitempty"`
}

// ListenControl creates the control socket at path. Its directory is
// created 0700 (an existing one is restricted to 0700; a link or file there
// is refused) and the socket is 0600, so only the proxy's user (and root on
// the host, through `docker exec`) can connect. A stale socket left by a
// previous process is replaced; any other file at path is an error.
func ListenControl(path string) (net.Listener, error) {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("control directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("control path %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale control socket: %w", err)
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("restrict control socket: %w", err)
	}
	return listener, nil
}

// ServeControl answers control requests until ctx ends or the listener
// fails. Each connection carries one JSON request and one JSON response.
func (s *Server) ServeControl(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleControl(ctx, conn)
	}
}

func (s *Server) handleControl(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
	var request ControlRequest
	decoder := json.NewDecoder(io.LimitReader(conn, maxControlRequest))
	decoder.DisallowUnknownFields()
	response := ControlResponse{}
	if err := decoder.Decode(&request); err != nil {
		response.Error = "invalid control request"
	} else {
		_ = conn.SetReadDeadline(time.Time{})
		response = s.dispatch(ctx, request)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(controlReadTimeout))
	_ = json.NewEncoder(conn).Encode(response)
}

func (s *Server) dispatch(ctx context.Context, request ControlRequest) ControlResponse {
	switch request.Op {
	case "deploy":
		if request.Deploy == nil {
			return ControlResponse{Error: "deploy request missing"}
		}
		result, err := s.Deploy(ctx, *request.Deploy)
		if err != nil {
			code := "invalid"
			if errors.Is(err, ErrUnhealthy) {
				code = "unhealthy"
			}
			return ControlResponse{Error: err.Error(), Code: code}
		}
		return ControlResponse{OK: true, Deploy: &result}
	case "remove":
		drain := time.Duration(request.DrainTimeout)
		if drain <= 0 {
			drain = DefaultDrainTimeout
		}
		if err := s.Remove(request.Service, drain); err != nil {
			return ControlResponse{Error: err.Error()}
		}
		return ControlResponse{OK: true}
	case "maintenance":
		if err := s.SetMaintenance(request.Service, request.Maintenance); err != nil {
			return ControlResponse{Error: err.Error()}
		}
		return ControlResponse{OK: true}
	case "status":
		return ControlResponse{OK: true, Services: s.Status()}
	default:
		return ControlResponse{Error: "unknown op"}
	}
}

// Call sends request to the control socket at path and returns the
// response. A response with OK false is returned without an error; err is
// set only when the exchange itself failed.
func Call(ctx context.Context, path string, request ControlRequest) (ControlResponse, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("connect to forge-proxy control socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return ControlResponse{}, fmt.Errorf("send control request: %w", err)
	}
	var response ControlResponse
	if err := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, maxControlRequest*16))).Decode(&response); err != nil {
		return ControlResponse{}, fmt.Errorf("read control response: %w", err)
	}
	return response, nil
}
