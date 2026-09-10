package teams

import (
	"fmt"
	"os/exec"
	"strings"
)

// ModeITerm 通过 AppleScript 把每个 teammate 拉到各自独立的 iTerm2 tab 里。
// 仅 macOS；当 ITERM_SESSION_ID 存在时 detectBackend 会选它。
const ModeITerm TeamMode = "iterm"

// spawnITermTeammate 打开一个新的 iTerm2 tab 并在里面运行 cliCommand。
// 返回脚本侧的 tab 标识（"team-member"），
// 这样调用方之后可以针对它做关闭。
func spawnITermTeammate(teamName, memberName, cliCommand string) (string, error) {
	tabName := fmt.Sprintf("%s-%s", teamName, memberName)
	// 把内嵌的双引号转义掉，保证 AppleScript 字符串字面量仍然合法。
	safeCmd := strings.ReplaceAll(cliCommand, `"`, `\"`)
	safeName := strings.ReplaceAll(tabName, `"`, `\"`)
	script := fmt.Sprintf(`tell application "iTerm2"
  tell current window
    set newTab to create tab with default profile
    tell newTab
      set name to "%s"
      tell current session
        write text "%s"
      end tell
    end tell
  end tell
end tell`, safeName, safeCmd)

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("osascript: %s: %s", err, strings.TrimSpace(string(out)))
	}
	return tabName, nil
}

// stopITermTeammate 关闭 spawnITermTeammate 创建的那个 iTerm2 tab。
// 尽力而为：tab 不存在 / 窗口已关闭都不算错误。
func stopITermTeammate(tabName string) {
	safeName := strings.ReplaceAll(tabName, `"`, `\"`)
	script := fmt.Sprintf(`tell application "iTerm2"
  repeat with w in windows
    repeat with t in tabs of w
      if name of t is "%s" then
        tell t to close
      end if
    end repeat
  end repeat
end tell`, safeName)
	exec.Command("osascript", "-e", script).Run()
}

