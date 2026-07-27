package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	policyFileName = "policy.json"
	quotaFileName  = "quota.json"
)

type stateStore struct {
	dir string
}

func newStateStore(dir string) *stateStore {
	return &stateStore{dir: filepath.Clean(dir)}
}

func (s *stateStore) policyExists() bool {
	if s == nil {
		return false
	}
	_, err := os.Stat(filepath.Join(s.dir, policyFileName))
	return err == nil
}

func (s *stateStore) loadPolicy() (PolicyDocument, error) {
	doc := defaultPolicyDocument()
	err := s.readJSON(policyFileName, &doc)
	if os.IsNotExist(err) {
		return doc, nil
	}
	if err != nil {
		return PolicyDocument{}, err
	}
	return normalizePolicyDocument(doc)
}

func (s *stateStore) savePolicy(doc PolicyDocument) error {
	normalized, err := normalizePolicyDocument(doc)
	if err != nil {
		return err
	}
	return s.writeJSON(policyFileName, normalized)
}

func (s *stateStore) loadQuota() (QuotaDocument, error) {
	doc := defaultQuotaDocument()
	err := s.readJSON(quotaFileName, &doc)
	if os.IsNotExist(err) {
		return doc, nil
	}
	if err != nil {
		return QuotaDocument{}, err
	}
	return normalizeQuotaDocument(doc)
}

func (s *stateStore) saveQuota(doc QuotaDocument) error {
	normalized, err := normalizeQuotaDocument(doc)
	if err != nil {
		return err
	}
	return s.writeJSON(quotaFileName, normalized)
}

func (s *stateStore) readQuotaBytes() ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, quotaFileName))
}

func (s *stateStore) readJSON(name string, dst any) error {
	raw, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return err
	}
	if errUnmarshal := json.Unmarshal(raw, dst); errUnmarshal != nil {
		return fmt.Errorf("decode %s: %w", name, errUnmarshal)
	}
	return nil
}

func (s *stateStore) writeJSON(name string, value any) error {
	if errMkdir := os.MkdirAll(s.dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create state directory: %w", errMkdir)
	}
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(value); errEncode != nil {
		return fmt.Errorf("encode %s: %w", name, errEncode)
	}

	temp, errCreate := os.CreateTemp(s.dir, "."+name+".*")
	if errCreate != nil {
		return fmt.Errorf("create temporary %s: %w", name, errCreate)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()
	if errChmod := temp.Chmod(0o600); errChmod != nil {
		return fmt.Errorf("chmod temporary %s: %w", name, errChmod)
	}
	if _, errWrite := temp.Write(payload.Bytes()); errWrite != nil {
		return fmt.Errorf("write temporary %s: %w", name, errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		return fmt.Errorf("sync temporary %s: %w", name, errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temporary %s: %w", name, errClose)
	}
	closed = true
	target := filepath.Join(s.dir, name)
	if errRename := os.Rename(tempPath, target); errRename != nil {
		return fmt.Errorf("replace %s: %w", name, errRename)
	}
	if errChmod := os.Chmod(target, 0o600); errChmod != nil {
		return fmt.Errorf("chmod %s: %w", name, errChmod)
	}
	dir, errOpenDir := os.Open(s.dir)
	if errOpenDir != nil {
		return fmt.Errorf("open state directory: %w", errOpenDir)
	}
	defer func() {
		_ = dir.Close()
	}()
	if errSyncDir := dir.Sync(); errSyncDir != nil {
		return fmt.Errorf("sync state directory: %w", errSyncDir)
	}
	return nil
}
