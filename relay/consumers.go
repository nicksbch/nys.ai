package main

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// consumer is a named, server-side cursor with a filter. Its reader gets the
// matching events after Cursor and acks them to move it forward.
type consumer struct {
	Topic   string    `json:"topic"`
	Type    string    `json:"type,omitempty"`
	Cursor  uint64    `json:"cursor"`
	Created time.Time `json:"created"`
}

func (c consumer) filter() filter {
	f, _ := parseFilter(c.Topic, c.Type, "") // validated when the consumer was saved
	return f
}

var errNoConsumer = errors.New("no such consumer")

// consumers keeps every consumer in one JSON file, rewritten atomically on
// each change.
type consumers struct {
	mu   sync.Mutex
	path string
	m    map[string]consumer
}

func openConsumers(path string) (*consumers, error) {
	c := &consumers{path: path, m: map[string]consumer{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	return c, json.Unmarshal(b, &c.m)
}

func (c *consumers) save() error {
	b, err := json.MarshalIndent(c.m, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *consumers) get(name string) (consumer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[name]
	return v, ok
}

func (c *consumers) all() map[string]consumer {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]consumer, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// put creates or updates a consumer. A nil cursor keeps an existing
// consumer's position (a new one starts at latest).
func (c *consumers) put(name, topic, typ string, cursor *uint64, latest uint64) (consumer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[name]
	if !ok {
		v = consumer{Cursor: latest, Created: time.Now().UTC()}
	}
	v.Topic, v.Type = topic, typ
	if cursor != nil {
		v.Cursor = *cursor
	}
	c.m[name] = v
	return v, c.save()
}

// ack moves a consumer's cursor forward to id. Acking an older id does nothing.
func (c *consumers) ack(name string, id uint64) (consumer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[name]
	if !ok {
		return v, errNoConsumer
	}
	if id <= v.Cursor {
		return v, nil
	}
	v.Cursor = id
	c.m[name] = v
	return v, c.save()
}

func (c *consumers) remove(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[name]; !ok {
		return errNoConsumer
	}
	delete(c.m, name)
	return c.save()
}
