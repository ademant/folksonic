package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/logging"
)

// idleWait is how long the worker sleeps when the server has no work.
const idleWait = time.Minute

// errRejected is returned when the server refuses the worker key; the worker
// stops, because retrying cannot help.
var errRejected = errors.New("the server rejected the worker key (revoked, expired or not a worker key)")

// Run asks the server for work until ctx is done. Each batch is analysed
// track by track; a result that fails to send is logged, and the lease then
// expires on the server.
func Run(ctx context.Context, cfg *Config) error {
	c := &client{base: strings.TrimSuffix(cfg.Server, "/"), key: cfg.APIKey,
		http: &http.Client{Timeout: 5 * time.Minute}}
	logging.Infof("worker: asking %s for folksentia work, batch %d", c.base, cfg.Batch)
	for ctx.Err() == nil {
		jobs, err := c.jobs(ctx, cfg.Batch)
		if errors.Is(err, errRejected) {
			return err
		}
		if err != nil {
			logging.Warnf("worker: %v; retrying in %s", err, idleWait)
			sleep(ctx, idleWait)
			continue
		}
		if len(jobs) == 0 {
			sleep(ctx, idleWait)
			continue
		}
		for _, j := range jobs {
			if ctx.Err() != nil {
				break
			}
			c.process(ctx, cfg, j)
		}
	}
	return nil
}

// job is one track as the server gives it out.
type job struct {
	ID         string   `json:"id"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	MtimeMs    int64    `json:"mtimeMs"`
	DurationMs int64    `json:"durationMs"`
	Features   []string `json:"features"`
}

type client struct {
	base string
	key  string
	http *http.Client
}

// do sends one request with the worker key and decodes a JSON answer into
// out. A 401 or 403 is errRejected; other errors carry the server's message.
func (c *client) do(ctx context.Context, path string, body, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if c.key != "" && (res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden) {
		return res.StatusCode, errRejected
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return res.StatusCode, fmt.Errorf("%s: %s", path, e.Error)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return res.StatusCode, fmt.Errorf("%s: %w", path, err)
		}
	}
	return res.StatusCode, nil
}

func (c *client) jobs(ctx context.Context, batch int) ([]job, error) {
	var out struct {
		Jobs []job `json:"jobs"`
	}
	if _, err := c.do(ctx, "/api/worker/jobs", map[string]any{"tool": "folksentia", "limit": batch}, &out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// process analyses one track and sends the result. A track whose file is
// missing or changed here is reported as an error, with the fingerprint it was
// given out with, so the server records it.
func (c *client) process(ctx context.Context, cfg *Config, j job) {
	res := map[string]any{"id": j.ID, "tool": "folksentia", "size": j.Size, "mtimeMs": j.MtimeMs}
	local, err := localPath(cfg.MusicRoot, j.Path)
	if err != nil {
		res["error"] = err.Error()
	} else if info, serr := os.Stat(local); serr != nil || info.Size() != j.Size || info.ModTime().UnixMilli() != j.MtimeMs {
		res["error"] = "file missing or changed on this worker"
	} else {
		tctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		out, rerr := analysis.RunFolksentia(tctx, cfg.Folksentia.Command, local, j.Features, cfg.memoryLimit)
		timedOut := tctx.Err() == context.DeadlineExceeded
		cancel()
		switch {
		case ctx.Err() != nil:
			// Stopped on purpose: send nothing; the lease runs out on the server.
			return
		case timedOut:
			res["error"] = fmt.Sprintf("timed out after %s", cfg.timeout)
		case rerr != nil:
			res["error"] = rerr.Error()
		default:
			res["output"] = string(out)
		}
	}
	start := time.Now()
	_, err = c.do(ctx, "/api/worker/results", res, nil)
	switch {
	case errors.Is(err, errRejected):
		logging.Errorf("worker: %v", err)
	case err != nil:
		logging.Warnf("worker: result for %s not stored: %v", j.ID, err)
	default:
		logging.Infof("worker: %s analysed in %s", j.ID, time.Since(start).Round(time.Millisecond))
	}
}

// localPath maps a server-relative path into the music root. A path that
// leaves the root is refused.
func localPath(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the music root", rel)
	}
	return filepath.Join(root, clean), nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Redeem exchanges a pairing token for a worker key at server. The returned
// key is for this worker only.
func Redeem(ctx context.Context, server, token, device string) (string, error) {
	c := &client{base: strings.TrimSuffix(server, "/"), http: &http.Client{Timeout: 30 * time.Second}}
	var out struct {
		APIKey string `json:"apiKey"`
	}
	if _, err := c.do(ctx, "/api/pair/complete", map[string]string{"token": token, "device": device}, &out); err != nil {
		return "", err
	}
	if out.APIKey == "" {
		return "", fmt.Errorf("the server sent no key")
	}
	return out.APIKey, nil
}
