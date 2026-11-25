package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
)

// Check a single candidate with the installed core before committing it to the
// shared snapshot. No listeners, credentials in argv, or changes to the live core.
func (r *runtime) validateOutbound(w http.ResponseWriter, req *http.Request) {
	var node egress.Node
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 160<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&node) != nil || dec.Decode(new(any)) != io.EOF || validateSnapshot(egress.Snapshot{Nodes: []egress.Node{node}}, r.key) != nil {
		http.Error(w, "invalid node", http.StatusBadRequest)
		return
	}
	select {
	case r.validationSlots <- struct{}{}:
		defer func() { <-r.validationSlots }()
	default:
		http.Error(w, "validation busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 4*time.Second)
	defer cancel()
	valid, err := checkOutbound(ctx, "/usr/local/bin/mihomo", workDirectory, node)
	if err != nil {
		http.Error(w, "validation unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"valid": valid})
}

func checkOutbound(ctx context.Context, binary, base string, node egress.Node) (bool, error) {
	config, err := marshalMihomo(map[string]*nodeRuntime{"candidate": {node: node, port: 20000}}, "preflight")
	if err != nil {
		return false, err
	}
	dir, err := os.MkdirTemp(base, "check-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, binary, "-t", "-d", dir, "-f", "-") // #nosec G204 -- fixed installed executable and flags; node data only travels over stdin.
	cmd.Stdin = bytes.NewReader(config)
	err = cmd.Run()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}
