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
	var st admissionState
	if err := json.Unmarshal(data, &st); err != nil {
		return admissionState{}, fmt.Errorf("admission state: malformed json: %w", err)
	}
	if st.Version != 1 {
		return admissionState{}, fmt.Errorf("admission state: unsupported version %d", st.Version)
	}
	return st, nil
}

func admissionAllowsScheduling(root string) bool {
	st, err := loadAdmissionState(root)
	return err == nil && !st.Paused
}

func setAdmissionPaused(root string, paused bool, actor, reason string) (admissionState, error) {
	var out admissionState
	err := withControlFileLock(root, "global-admission", func() error {
		st, err := loadAdmissionState(root)
		if err != nil {
			return err
		}
		if st.Paused != paused {
			if st.Epoch == math.MaxUint64 {
				return fmt.Errorf("admission state: epoch overflow")
			}
			st.Epoch++
			st.Paused = paused
		}
		st.Version = 1
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
