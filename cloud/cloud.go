// Package cloud simulates a cloud provider on the local filesystem: each
// resource is a JSON file named "<type>.<name>.json" inside a directory.
// It supports failure injection so tests can exercise mid-apply crashes.
package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound is returned when a resource does not exist in the cloud.
var ErrNotFound = errors.New("resource not found in cloud")

// ErrExists is returned when creating a resource that already exists.
var ErrExists = errors.New("resource already exists in cloud")

// Instance is a live cloud resource.
type Instance struct {
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Properties map[string]any `json:"properties"`
}

// Client is a file-backed simulated cloud. It is safe for concurrent use.
type Client struct {
	dir string
	mu  sync.Mutex

	// FailOnce maps "op:type.name" to an error returned exactly once for
	// that operation (before it is applied). Used by tests to simulate
	// crashes mid-apply.
	FailOnce map[string]error
}

// New creates (if needed) a cloud rooted at dir.
func New(dir string) *Client {
	return &Client{dir: dir, FailOnce: map[string]error{}}
}

func (c *Client) path(addr string) string { return filepath.Join(c.dir, addr+".json") }

// InjectFailure makes the next op ("create"/"update"/"delete") on addr fail
// with err, exactly once.
func (c *Client) InjectFailure(op, addr string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FailOnce[op+":"+addr] = err
}

func (c *Client) maybeFail(op, addr string) error {
	if err, ok := c.FailOnce[op+":"+addr]; ok {
		delete(c.FailOnce, op+":"+addr)
		return err
	}
	return nil
}

// Create creates a new resource; fails with ErrExists if already present.
func (c *Client) Create(inst Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr := inst.Type + "." + inst.Name
	if err := c.maybeFail("create", addr); err != nil {
		return err
	}
	if _, err := os.Stat(c.path(addr)); err == nil {
		return fmt.Errorf("%s: %w", addr, ErrExists)
	}
	return c.writeLocked(inst)
}

// Read fetches a resource; fails with ErrNotFound if absent.
func (c *Client) Read(typ, name string) (Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr := typ + "." + name
	data, err := os.ReadFile(c.path(addr))
	if errors.Is(err, os.ErrNotExist) {
		return Instance{}, fmt.Errorf("%s: %w", addr, ErrNotFound)
	}
	if err != nil {
		return Instance{}, err
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return Instance{}, fmt.Errorf("corrupt cloud object %s: %w", addr, err)
	}
	return inst, nil
}

// Update replaces a resource's properties; fails with ErrNotFound if absent.
func (c *Client) Update(inst Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr := inst.Type + "." + inst.Name
	if err := c.maybeFail("update", addr); err != nil {
		return err
	}
	if _, err := os.Stat(c.path(addr)); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", addr, ErrNotFound)
	}
	return c.writeLocked(inst)
}

// Delete removes a resource; fails with ErrNotFound if absent.
func (c *Client) Delete(typ, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr := typ + "." + name
	if err := c.maybeFail("delete", addr); err != nil {
		return err
	}
	err := os.Remove(c.path(addr))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", addr, ErrNotFound)
	}
	return err
}

func (c *Client) writeLocked(inst Instance) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write via temp file + rename.
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path(inst.Type+"."+inst.Name))
}
