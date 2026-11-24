package sys

import (
	"context"
	"fmt"
	"strings"
)

type RctlManager struct {
	runner Runner
}

func NewRctlManager(runner Runner) *RctlManager {
	return &RctlManager{runner: runner}
}

func (r *RctlManager) IsEnabled(ctx context.Context) bool {
	res, err := r.runner.Run(ctx, "sysctl", "-n", "kern.racct.enable")
	if err != nil {
		return false
	}
	return strings.TrimSpace(res.Stdout) == "1"
}

func (r *RctlManager) ApplyLimits(ctx context.Context, jailName, memory, cpu string) error {
	if memory == "" && cpu == "" {
		return nil
	}
	if !r.IsEnabled(ctx) {
		return fmt.Errorf("rctl requested (memory=%q cpu=%q) but kern.racct.enable != 1", memory, cpu)
	}
	if memory != "" {
		if _, err := r.runner.Run(ctx, "rctl", "-a", fmt.Sprintf("jail:%s:memoryuse:deny=%s", jailName, memory)); err != nil {
			return err
		}
	}
	if cpu != "" {
		pct := strings.TrimSuffix(strings.TrimSpace(cpu), "%")
		if _, err := r.runner.Run(ctx, "rctl", "-a", fmt.Sprintf("jail:%s:pcpu:deny=%s", jailName, pct)); err != nil {
			return err
		}
	}
	return nil
}

func (r *RctlManager) ClearLimits(ctx context.Context, jailName string) error {
	if !r.IsEnabled(ctx) {
		return nil
	}
	_, err := r.runner.Run(ctx, "rctl", "-r", fmt.Sprintf("jail:%s", jailName))
	return err
}
