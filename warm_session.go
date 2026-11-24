package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aerokube/selenoid/jsonerror"
)

// Retain the slot until the driver is ready for the next session.
func deleteReusableSession(w http.ResponseWriter, id string) {
	s, ok := sessions.Get(id)
	if !ok {
		jsonerror.InvalidSessionID(errors.New("unknown session")).Encode(w)
		return
	}
	s.Lock.Lock()
	if s.Lifetime.Err() != nil {
		s.Lock.Unlock()
		jsonerror.InvalidSessionID(errors.New("session is ending")).Encode(w)
		return
	}
	s.End()
	select {
	case <-s.TimeoutCh:
	default:
		close(s.TimeoutCh)
	}
	s.Lock.Unlock()
	defer queue.Release()

	ctx, cancel := context.WithTimeout(context.Background(), sessionDeleteTimeout)
	defer cancel()
	endpoint := s.URL.JoinPath("session", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err == nil {
		req.Host = s.Origin
		err = endWebDriverSession(req)
	}
	sessions.Remove(id)
	s.Cancel()
	if enableFileUpload {
		_ = os.RemoveAll(filepath.Join(os.TempDir(), id))
	}
	if err := s.ReleaseContainer(err); err != nil {
		jsonerror.UnknownError(err).Encode(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"value":null}`)
}

func endWebDriverSession(req *http.Request) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete WebDriver session: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		Value  json.RawMessage `json:"value"`
		Status int             `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&result) != nil ||
		result.Status != 0 || string(result.Value) != "null" {
		return fmt.Errorf("WebDriver did not confirm session deletion (HTTP %d)", resp.StatusCode)
	}
	return nil
}
