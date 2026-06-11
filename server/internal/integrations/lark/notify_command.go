package lark

import "strings"

// notifyCommandPrefix is the literal command token. Exact-case match,
// same rationale as issueCommandPrefix: `/Notify` inline in a sentence
// must not flip workspace notification routing.
const notifyCommandPrefix = "/notify"

// NotifyAction is the parsed verdict of a /notify command.
type NotifyAction string

const (
	// NotifyActionOn — set the current group chat as the
	// installation's inbox-notification group.
	NotifyActionOn NotifyAction = "on"
	// NotifyActionOff — clear the installation's notification group;
	// inbox notifications fall back to DM'ing bound recipients.
	NotifyActionOff NotifyAction = "off"
	// NotifyActionHelp — `/notify` alone or with an unrecognized
	// argument. The replier answers with usage copy instead of
	// guessing the user's intent.
	NotifyActionHelp NotifyAction = "help"
)

// parseNotifyCommand extracts a /notify command from a chat-message
// body. Mirrors parseIssueCommand's shape rules: only the first
// non-empty line is considered, the prefix must be a whole token
// (`/notifyme` does not match), and matching is case-sensitive.
//
// Recognized shapes:
//
//   - `/notify on`   → NotifyActionOn
//   - `/notify off`  → NotifyActionOff
//   - `/notify` or `/notify <anything else>` → NotifyActionHelp
func parseNotifyCommand(body string) (NotifyAction, bool) {
	lines := strings.Split(body, "\n")

	firstIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			firstIdx = i
			break
		}
	}
	if firstIdx == -1 {
		return "", false
	}

	trimmed := strings.TrimLeft(lines[firstIdx], " \t")
	if !strings.HasPrefix(trimmed, notifyCommandPrefix) {
		return "", false
	}
	rest := trimmed[len(notifyCommandPrefix):]
	if rest != "" {
		r0 := rest[0]
		if r0 != ' ' && r0 != '\t' {
			return "", false
		}
	}

	switch strings.ToLower(strings.TrimSpace(rest)) {
	case "on":
		return NotifyActionOn, true
	case "off":
		return NotifyActionOff, true
	default:
		return NotifyActionHelp, true
	}
}
