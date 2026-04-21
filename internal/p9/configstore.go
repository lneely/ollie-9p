package p9

import (
	"os"

	"ollie/pkg/agent"
	"ollie/pkg/config"
)

// loadAgentConfig resolves and loads the config for a named agent.
// Returns nil (not an error) if the config file does not exist;
// BuildAgentEnv handles nil configs.
func loadAgentConfig(agentsDir, name string) *config.Config {
	f, err := os.Open(agent.AgentConfigPath(agentsDir, name))
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, _ := config.Load(f)
	return cfg
}
