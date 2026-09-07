package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestTerminalExecutionDisabled(t *testing.T) {
	t.Setenv("DISABLE_EXECUTE", "1")
	if !terminalExecutionDisabled() {
		t.Fatal("DISABLE_EXECUTE=1 did not disable the terminal relay")
	}
	t.Setenv("DISABLE_EXECUTE", "0")
	if terminalExecutionDisabled() {
		t.Fatal("DISABLE_EXECUTE=0 disabled the terminal relay")
	}
}

func TestInstallerRestartsAndRollsBackTerminalService(t *testing.T) {
	script, err := os.ReadFile("../script/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, contract := range []string{
		"ExecStart=/usr/local/v2node/v2node terminal",
		"systemctl restart v2node-terminal",
		"systemctl is-active --quiet v2node-terminal",
		"service v2node-terminal restart",
		"service v2node-terminal status",
		"systemctl stop v2node-terminal",
		"service v2node-terminal stop",
		"if ! start_terminal_service; then",
		"rollback_activated_runtime \"$had_previous\"",
		"DISABLE_EXECUTE_WAS_SET",
		"/etc/v2node/terminal.env",
		"runtime_supports_terminal",
		"remove_terminal_service",
		"rm -f /etc/systemd/system/v2node-terminal.service",
		"rm -f /etc/init.d/v2node-terminal",
	} {
		if !strings.Contains(contents, contract) {
			t.Fatalf("installer missing terminal lifecycle contract %q", contract)
		}
	}
	if strings.Contains(contents, "enable --now v2node-terminal") {
		t.Fatal("installer starts terminal before config and runtime validation")
	}
}

func TestManagerKeepsTerminalServiceInMainRuntimeLifecycle(t *testing.T) {
	script, err := os.ReadFile("../script/v2node.sh")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, contract := range []string{
		"runtime_supports_terminal",
		"install_terminal_service_unit",
		"start_terminal_service",
		"stop_v2node_service",
		"remove_terminal_service",
		"systemctl is-active --quiet v2node-terminal",
		"service v2node-terminal status",
	} {
		if !strings.Contains(contents, contract) {
			t.Fatalf("manager missing terminal lifecycle contract %q", contract)
		}
	}
}
