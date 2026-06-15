package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var poolIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// GlobalInstance mirrors one enabled entry in admin_token_config_global.json.
// Sensitive fields are parsed only so the file shape can be validated; logs
// must not print RefreshToken or Proxy values.
type GlobalInstance struct {
	PoolID         string `json:"pool_id"`
	RefreshToken   string `json:"refresh_token"`
	TenantID       string `json:"tenant_id"`
	Username       string `json:"username"`
	Domain         string `json:"domain"`
	Proxy          string `json:"proxy"`
	EmailFile      string `json:"email_file"`
	BotPrefix      string `json:"bot_prefix"`
	CheckpointFile string `json:"checkpoint_file"`
	ResultFile     string `json:"result_file"`
	Workers        int    `json:"workers"`
	MaxCPM         int    `json:"max_cpm"`
	Enabled        *bool  `json:"enabled"`
	resolvedPoolID string
}

// LoadGlobalInstances loads all enabled pool instances from the global config.
func LoadGlobalInstances(path string) ([]GlobalInstance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read global config %s: %w", path, err)
	}

	var raw []GlobalInstance
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse global config %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("global config %s must contain at least one instance", path)
	}

	instances := make([]GlobalInstance, 0, len(raw))
	seenPools := make(map[string]struct{}, len(raw))
	seenCheckpoints := make(map[string]struct{}, len(raw))
	for idx, instance := range raw {
		if instance.Enabled != nil && !*instance.Enabled {
			continue
		}
		if err := instance.validate(idx); err != nil {
			return nil, err
		}
		poolID := instance.resolvedPoolID
		if _, ok := seenPools[poolID]; ok {
			return nil, fmt.Errorf("duplicate pool_id %q in global config", poolID)
		}
		seenPools[poolID] = struct{}{}

		checkpoint := instance.CheckpointFile
		if checkpoint == "" {
			checkpoint = DefaultCheckpointPath(instance.EmailFile, poolID)
		}
		checkpointKey := filepath.Clean(checkpoint)
		if _, ok := seenCheckpoints[checkpointKey]; ok {
			return nil, fmt.Errorf("duplicate checkpoint path %q in global config", checkpoint)
		}
		seenCheckpoints[checkpointKey] = struct{}{}

		instances = append(instances, instance)
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("global config %s has no enabled instances", path)
	}
	return instances, nil
}

// SelectGlobalInstance returns the requested pool. If requested is empty, it
// only succeeds when exactly one enabled instance exists.
func SelectGlobalInstance(instances []GlobalInstance, requested string) (GlobalInstance, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(instances) == 1 {
			return instances[0], nil
		}
		return GlobalInstance{}, errors.New("multiple instances configured; pass --pool")
	}

	for _, instance := range instances {
		if instance.ResolvedPoolID() == requested || instance.PoolID == requested || instance.BotPrefix == requested {
			return instance, nil
		}
	}
	return GlobalInstance{}, fmt.Errorf("pool %q not found in global config", requested)
}

// ResolvedPoolID returns the safe pool identifier used in API paths and file names.
func (i GlobalInstance) ResolvedPoolID() string {
	if i.resolvedPoolID != "" {
		return i.resolvedPoolID
	}
	poolID, _ := resolvePoolID(i)
	return poolID
}

func (i *GlobalInstance) validate(idx int) error {
	missing := make([]string, 0, 3)
	if strings.TrimSpace(i.EmailFile) == "" {
		missing = append(missing, "email_file")
	}
	if strings.TrimSpace(i.BotPrefix) == "" {
		missing = append(missing, "bot_prefix")
	}
	if strings.TrimSpace(i.Domain) == "" {
		missing = append(missing, "domain")
	}
	if len(missing) > 0 {
		return fmt.Errorf("global config entry #%d missing required field(s): %s", idx, strings.Join(missing, ", "))
	}

	poolID, err := resolvePoolID(*i)
	if err != nil {
		return fmt.Errorf("global config entry #%d: %w", idx, err)
	}
	i.resolvedPoolID = poolID
	return nil
}

func resolvePoolID(instance GlobalInstance) (string, error) {
	if poolID := strings.TrimSpace(instance.PoolID); poolID != "" {
		return validatePoolID(poolID)
	}
	candidate := strings.TrimRight(strings.TrimSpace(instance.BotPrefix), "_")
	if candidate == "" {
		candidate = strings.TrimSpace(instance.Username)
	}
	if candidate == "" {
		return "", errors.New("pool_id is required when bot_prefix is empty")
	}
	return validatePoolID(candidate)
}

func validatePoolID(poolID string) (string, error) {
	if !poolIDPattern.MatchString(poolID) {
		return "", fmt.Errorf("invalid pool_id %q; use only letters, numbers, '.', '_', '-'", poolID)
	}
	return poolID, nil
}

// DefaultGlobalConfigPath finds admin_token_config_global.json without falling
// back to the legacy admin_token.json.
func DefaultGlobalConfigPath() (string, error) {
	candidates := []string{
		"admin_token_config_global.json",
		filepath.Join("..", "admin_token_config_global.json"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("admin_token_config_global.json not found in current directory or parent")
}

func DefaultCheckpointPath(emailFile string, poolID string) string {
	return fmt.Sprintf("%s.%s.ckpt", emailFile, poolID)
}

func DefaultResultPath(poolID string, timestamp string) string {
	return fmt.Sprintf("result_%s_%s.txt", poolID, timestamp)
}
