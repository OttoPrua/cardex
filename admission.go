package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
)

type admissionState struct {
	Version   int    `json:"version"`
	Epoch     uint64 `json:"epoch"`
	Paused    bool   `json:"paused"`
	Actor     string `json:"actor,omitempty"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

type admissionStateWire struct {
	Version   *int    `json:"version"`
	Epoch     *uint64 `json:"epoch"`
	Paused    *bool   `json:"paused"`
	Actor     *string `json:"actor,omitempty"`
	Reason    *string `json:"reason,omitempty"`
	UpdatedAt *string `json:"updated_at"`
}

func admissionPath(root string) string {
	return filepath.Join(controlDir(root), "admission.json")
}

func loadAdmissionState(root string) (admissionState, error) {
	data, err := os.ReadFile(admissionPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return admissionState{Version: 1}, nil
		}
		return admissionState{}, fmt.Errorf("admission state: read: %w", err)
	}
	wire, err := decodeClosedAdmissionV1(data)
	if err != nil {
		return admissionState{}, err
	}
	if wire.Version == nil {
		return admissionState{}, fmt.Errorf("admission state: missing version")
	}
	if *wire.Version != 1 {
		return admissionState{}, fmt.Errorf("admission state: unsupported version %d", *wire.Version)
	}
	if wire.Epoch == nil {
		return admissionState{}, fmt.Errorf("admission state: missing epoch")
	}
	if wire.Paused == nil {
		return admissionState{}, fmt.Errorf("admission state: missing paused")
	}
	if wire.UpdatedAt == nil {
		return admissionState{}, fmt.Errorf("admission state: missing updated_at")
	}
	if _, err := time.Parse(time.RFC3339Nano, *wire.UpdatedAt); err != nil {
		return admissionState{}, fmt.Errorf("admission state: invalid updated_at: %w", err)
	}
	st := admissionState{
		Version:   *wire.Version,
		Epoch:     *wire.Epoch,
		Paused:    *wire.Paused,
		UpdatedAt: *wire.UpdatedAt,
	}
	if wire.Actor != nil {
		st.Actor = *wire.Actor
	}
	if wire.Reason != nil {
		st.Reason = *wire.Reason
	}
	return st, nil
}

func decodeClosedAdmissionV1(data []byte) (admissionStateWire, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return admissionStateWire{}, fmt.Errorf("admission state: malformed json: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return admissionStateWire{}, fmt.Errorf("admission state: top-level value must be an object")
	}

	var wire admissionStateWire
	seen := make(map[string]struct{})
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return admissionStateWire{}, fmt.Errorf("admission state: malformed json: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return admissionStateWire{}, fmt.Errorf("admission state: malformed json: non-string field name")
		}
		field, ok := canonicalAdmissionField(key)
		if !ok {
			return admissionStateWire{}, fmt.Errorf("admission state: unknown field %q", key)
		}
		if _, dup := seen[field]; dup {
			return admissionStateWire{}, fmt.Errorf("admission state: duplicate field %q", key)
		}
		seen[field] = struct{}{}
		if err := decodeAdmissionField(dec, &wire, field); err != nil {
			return admissionStateWire{}, err
		}
	}
	end, err := dec.Token()
	if err != nil {
		return admissionStateWire{}, fmt.Errorf("admission state: malformed json: %w", err)
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return admissionStateWire{}, fmt.Errorf("admission state: malformed json: expected end of object")
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return admissionStateWire{}, fmt.Errorf("admission state: trailing json")
		}
		return admissionStateWire{}, fmt.Errorf("admission state: malformed json: %w", err)
	}
	return wire, nil
}

func canonicalAdmissionField(name string) (string, bool) {
	switch name {
	case "version", "epoch", "paused", "actor", "reason", "updated_at":
		return name, true
	default:
		return "", false
	}
}

func decodeAdmissionField(dec *json.Decoder, wire *admissionStateWire, field string) error {
	var err error
	switch field {
	case "version":
		err = dec.Decode(&wire.Version)
	case "epoch":
		err = dec.Decode(&wire.Epoch)
	case "paused":
		err = dec.Decode(&wire.Paused)
	case "actor":
		err = dec.Decode(&wire.Actor)
		if err == nil && wire.Actor == nil {
			return fmt.Errorf("admission state: actor must be a json string")
		}
	case "reason":
		err = dec.Decode(&wire.Reason)
		if err == nil && wire.Reason == nil {
			return fmt.Errorf("admission state: reason must be a json string")
		}
	case "updated_at":
		err = dec.Decode(&wire.UpdatedAt)
	default:
		return fmt.Errorf("admission state: unknown field %q", field)
	}
	if err != nil {
		return fmt.Errorf("admission state: malformed json: %w", err)
	}
	return nil
}

func admissionAllowsScheduling(root string) bool {
	st, err := loadAdmissionState(root)
	return err == nil && !st.Paused
}

func setAdmissionPaused(root string, paused bool, actor, reason string) (admissionState, error) {
	var out admissionState
	err := withTaskControlLock(root, "global-admission", func() error {
		st, err := loadAdmissionState(root)
		if err != nil {
			return err
		}
		if st.Paused == paused {
			out = st
			return nil
		}
		if st.Epoch == math.MaxUint64 {
			return fmt.Errorf("admission state: epoch overflow")
		}
		st.Version = 1
		st.Epoch++
		st.Paused = paused
		st.Actor = actor
		st.Reason = reason
		st.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		data, err := json.Marshal(st)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := atomicWriteSync(admissionPath(root), data); err != nil {
			return err
		}
		out = st
		return nil
	})
	if err != nil {
		return admissionState{}, err
	}
	return out, nil
}
