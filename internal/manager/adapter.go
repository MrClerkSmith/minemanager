// Bridging helpers between the manager and the scheduler.
package manager

import (
	"bytes"
	"encoding/json"
	"errors"

	"mineserver/internal/instance"
	"mineserver/internal/sched"
)

// ErrNotFound is returned for unknown server ids.
var ErrNotFound = errors.New("server not found")

// instanceAdapter makes an *instance.Instance satisfy sched.Runnable.
type instanceAdapter struct {
	*instance.Instance
}

// Schedule converts the stored schedule into the scheduler's own type.
func (a instanceAdapter) Schedule() *sched.Schedule {
	s := a.Instance.Cfg().Schedule
	if s == nil {
		return nil
	}
	return &sched.Schedule{
		RestartEvery: s.RestartEvery,
		BackupEvery:  s.BackupEvery,
		RestartAt:    s.RestartAt,
		BackupAt:     s.BackupAt,
	}
}

// strictUnmarshal rejects unknown fields so typos in a saved config surface
// instead of being silently dropped.
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
