package main

import (
	"encoding/json"
	"fmt"
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
	Actor     string  `json:"actor,omitempty"`
	Reason    string  `json:"reason,omitempty"`
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
	var wire admissionStateWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return admissionState{}, fmt.Errorf("admission state: malformed json: %w", err)
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
	return admissionState{
		Version:   *wire.Version,
		Epoch:     *wire.Epoch,
		Paused:    *wire.Paused,
		Actor:     wire.Actor,
		Reason:    wire.Reason,
		UpdatedAt: *wire.UpdatedAt,
	}, nil
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
