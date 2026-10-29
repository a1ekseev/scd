// Package wgpstatus is the status.json v1 format that wgp writes next to its
// candidate outputs and scd reads. It holds only fields common to every
// provider and no secrets: device ids, if ever shown, appear only as sha8.
package wgpstatus

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/a1ekseev/scd/internal/common/securefile"
)

// Name is the status file name inside the wgp output directory.
const Name = "status.json"

// Version is the only supported format version. Later changes are additive.
const Version = 1

// Provider states.
const (
	StateOK         = "ok"
	StateNeedsLogin = "needs_login"
	StateError      = "error"
)

// Status is the whole status.json document.
type Status struct {
	Version   int        `json:"version"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Providers []Provider `json:"providers"`
}

// Provider is the status of one provider and its output.
type Provider struct {
	// ID is the provider id from the wgp config.
	ID string `json:"id"`
	// Output is the id of the candidate output the provider writes.
	Output string `json:"output"`
	// Type is the provider type, a free-form string.
	Type string `json:"type"`
	// State is one of StateOK, StateNeedsLogin or StateError.
	State string `json:"state"`
	// Error is the last error text, without secrets or response bodies.
	Error string `json:"error,omitempty"`
	// LastTickAt is the start of the last tick.
	LastTickAt time.Time `json:"lastTickAt,omitzero"`
	// LastSuccessAt is the start of the last tick that ended in StateOK.
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`
	// Issued is the number of candidates issued.
	Issued int `json:"issued"`
	// Limit is the plan limit on issued candidates, 0 when unknown. Information only.
	Limit int `json:"limit,omitzero"`
	// ExpiresAt is the earliest expiry among the issued candidates, zero when they do not expire.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

// Read reads and parses dir/status.json through securefile.ReadPrivate.
// Unknown fields are ignored; a version other than Version is an error.
func Read(dir string, maxBytes int64) (Status, error) {
	data, err := securefile.ReadPrivate(filepath.Join(dir, Name), maxBytes)
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("parse %s: %w", Name, err)
	}
	if s.Version != Version {
		return Status{}, fmt.Errorf("%s: unsupported version %d", Name, s.Version)
	}
	return s, nil
}

// Write writes s to dir/status.json atomically with mode 0600. It sets
// s.Version to Version.
func Write(dir string, s Status) error {
	s.Version = Version
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(filepath.Join(dir, Name), append(data, '\n'))
}
