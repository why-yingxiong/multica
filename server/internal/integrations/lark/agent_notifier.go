package lark

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AgentNotifierQueries is the narrow subset of *db.Queries the
// AgentNotifier needs. *db.Queries satisfies it directly; tests
// substitute a fake.
type AgentNotifierQueries interface {
	GetLarkInstallationByAgent(ctx context.Context, arg db.GetLarkInstallationByAgentParams) (db.LarkInstallation, error)
	GetLarkUserBindingByUser(ctx context.Context, arg db.GetLarkUserBindingByUserParams) (db.LarkUserBinding, error)
	// GetIssue + GetWorkspace render the "SMO-42「title」" reference for
	// mention notices, whose event payload only carries the issue UUID.
	// Either lookup failing degrades the reference, never the notice.
	GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error)
	GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error)
}

// AgentNotifierConfig tunes the notifier.
type AgentNotifierConfig struct {
	// PublicURL is the Multica HTTP host used for the issue deep link
	// ("https://multica.example"). Empty omits the link.
	PublicURL string
	Logger    *slog.Logger
}

func (c AgentNotifierConfig) withDefaults() AgentNotifierConfig {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	return c
}

// agentNotifyTimeout bounds one notice delivery (two DB lookups,
// one Lark HTTP send, one transcript append). The work runs detached
// from the publishing goroutine.
const agentNotifyTimeout = 10 * time.Second

// IssueAssignedNotice carries one "an agent created an issue and
// assigned it to a member" fact. The wiring layer (cmd/server) builds
// it from the EventIssueCreated payload; this package never sees the
// handler's response types.
type IssueAssignedNotice struct {
	WorkspaceID    pgtype.UUID
	AgentID        pgtype.UUID // the creating agent
	AssigneeUserID pgtype.UUID // the member the issue was assigned to
	Identifier     string      // workspace-qualified key, e.g. "MUL-42"
	Title          string
}

// AgentNotifier tells an issue's assignee — in conversation —
// that an agent created an issue for them. This replaces the earlier
// inbox→Lark bridge idea (notifications fired outside any chat
// session, so the agent could not see what its own bot had told the
// user). Instead:
//
//   - The notice is sent through the CREATING agent's own Bot, as a
//     plain text DM to the assignee's open_id. It reads as the agent
//     speaking, because it is.
//
//   - The send response's chat_id identifies the assignee↔Bot p2p
//     chat. The notifier ensures the matching Multica chat_session
//     exists and mirrors the notice into the transcript as an
//     assistant message — so when the user replies "这个 issue 是谁给
//     我的", the agent's context already contains the answer.
//
// Delivery is strictly best-effort: no installation for the agent, an
// unbound assignee, or a Lark outage all degrade to a debug/warn log.
// The issue itself (and its in-app inbox notification) is already
// durable. Multi-replica safety is the Patcher's argument: the bus is
// per-process and EventIssueCreated is published on exactly one
// replica.
type AgentNotifier struct {
	queries     AgentNotifierQueries
	credentials CredentialsResolver
	client      APIClient
	chat        ChatSessionService
	cfg         AgentNotifierConfig

	// dispatch is the seam tests use to run deliveries inline. In
	// production it detaches a goroutine so issue creation never waits
	// on a Lark HTTP round-trip.
	dispatch func(func())
}

// NewAgentNotifier constructs an AgentNotifier bound to its
// dependencies.
func NewAgentNotifier(
	queries AgentNotifierQueries,
	credentials CredentialsResolver,
	client APIClient,
	chat ChatSessionService,
	cfg AgentNotifierConfig,
) *AgentNotifier {
	return &AgentNotifier{
		queries:     queries,
		credentials: credentials,
		client:      client,
		chat:        chat,
		cfg:         cfg.withDefaults(),
		dispatch:    func(fn func()) { go fn() },
	}
}

// MentionNotice carries one "an agent @-mentioned a member in an
// issue comment" fact. Unlike IssueAssignedNotice, the wiring layer
// only has the issue's UUID (the comment payload does not carry the
// identifier/title), so the notifier resolves those itself and
// degrades gracefully when the lookups fail.
type MentionNotice struct {
	WorkspaceID     pgtype.UUID
	AgentID         pgtype.UUID // the commenting agent
	MentionedUserID pgtype.UUID
	IssueID         pgtype.UUID
	CommentBody     string
}

// NotifyAssigned delivers one assignment notice asynchronously. Call
// sites pass already-validated facts (creator is an agent, assignee is
// a member); the notifier owns the Lark-side checks.
func (n *AgentNotifier) NotifyAssigned(notice IssueAssignedNotice) {
	n.dispatch(func() {
		ctx, cancel := context.WithTimeout(context.Background(), agentNotifyTimeout)
		defer cancel()
		text := assignmentNoticeText(notice, n.cfg.PublicURL)
		if err := n.deliver(ctx, notice.WorkspaceID, notice.AgentID, notice.AssigneeUserID, text); err != nil {
			n.cfg.Logger.Warn("lark agent notifier: assignment notice failed",
				"workspace_id", uuidString(notice.WorkspaceID),
				"agent_id", uuidString(notice.AgentID),
				"recipient_user_id", uuidString(notice.AssigneeUserID),
				"issue", notice.Identifier,
				"error", err,
			)
		}
	})
}

// NotifyMentioned delivers one comment-mention notice asynchronously.
// Call sites pass already-validated facts (the comment author is an
// agent, the mention targets a member).
func (n *AgentNotifier) NotifyMentioned(notice MentionNotice) {
	n.dispatch(func() {
		ctx, cancel := context.WithTimeout(context.Background(), agentNotifyTimeout)
		defer cancel()
		text := n.mentionNoticeText(ctx, notice)
		if err := n.deliver(ctx, notice.WorkspaceID, notice.AgentID, notice.MentionedUserID, text); err != nil {
			n.cfg.Logger.Warn("lark agent notifier: mention notice failed",
				"workspace_id", uuidString(notice.WorkspaceID),
				"agent_id", uuidString(notice.AgentID),
				"recipient_user_id", uuidString(notice.MentionedUserID),
				"issue_id", uuidString(notice.IssueID),
				"error", err,
			)
		}
	})
}

// deliver is the shared delivery core: resolve the agent's Bot,
// resolve the recipient's binding on it, DM the text, and mirror it
// into the p2p chat_session transcript. All skip conditions (no Bot,
// revoked installation, unbound recipient) return nil — the in-app
// surface already has the notification.
func (n *AgentNotifier) deliver(ctx context.Context, workspaceID, agentID, recipientUserID pgtype.UUID, text string) error {
	inst, err := n.queries.GetLarkInstallationByAgent(ctx, db.GetLarkInstallationByAgentParams{
		WorkspaceID: workspaceID,
		AgentID:     agentID,
	})
	if err != nil {
		if isNoRowsErr(err) {
			// The agent has no Lark Bot — nothing to deliver through.
			return nil
		}
		return fmt.Errorf("load installation: %w", err)
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		return nil
	}

	binding, err := n.queries.GetLarkUserBindingByUser(ctx, db.GetLarkUserBindingByUserParams{
		InstallationID: inst.ID,
		MulticaUserID:  recipientUserID,
	})
	if err != nil {
		if isNoRowsErr(err) {
			n.cfg.Logger.Debug("lark agent notifier: recipient not bound; skipped",
				"installation_id", uuidString(inst.ID),
				"recipient_user_id", uuidString(recipientUserID),
			)
			return nil
		}
		return fmt.Errorf("load recipient binding: %w", err)
	}

	creds, err := n.installationCredentials(inst)
	if err != nil {
		return err
	}

	res, err := n.client.SendUserTextMessage(ctx, SendUserTextParams{
		InstallationID: creds,
		OpenID:         OpenID(binding.LarkOpenID),
		Text:           text,
	})
	if err != nil {
		return fmt.Errorf("send notice: %w", err)
	}

	// Mirror the notice into the conversation transcript so it is part
	// of the agent's context on the next run. Best-effort: the user
	// already has the message in Lark; a transcript miss only costs
	// context, never the notification itself.
	if res.ChatID == "" {
		n.cfg.Logger.Warn("lark agent notifier: send response missing chat_id; notice not mirrored to transcript",
			"installation_id", uuidString(inst.ID),
		)
		return nil
	}
	sessionID, err := n.chat.EnsureChatSession(ctx, EnsureChatSessionParams{
		WorkspaceID:    inst.WorkspaceID,
		InstallationID: inst.ID,
		AgentID:        inst.AgentID,
		ChatID:         res.ChatID,
		ChatType:       ChatTypeP2P,
		Sender:         recipientUserID,
	})
	if err != nil {
		n.cfg.Logger.Warn("lark agent notifier: ensure chat session failed; notice not mirrored to transcript",
			"installation_id", uuidString(inst.ID),
			"chat_id", string(res.ChatID),
			"error", err,
		)
		return nil
	}
	if err := n.chat.AppendAgentMessage(ctx, sessionID, text); err != nil {
		n.cfg.Logger.Warn("lark agent notifier: transcript append failed",
			"chat_session_id", uuidString(sessionID),
			"error", err,
		)
	}
	return nil
}

// assignmentNoticeText composes the agent-voiced notice. Identifier is
// required by the caller; Title may be empty. PublicURL is optional —
// without it the message still names the issue, just without a
// tappable deep link.
func assignmentNoticeText(notice IssueAssignedNotice, publicURL string) string {
	title := strings.TrimSpace(notice.Title)
	var line string
	if title == "" {
		line = fmt.Sprintf("我创建了 %s 并指派给你。", notice.Identifier)
	} else {
		line = fmt.Sprintf("我创建了 %s「%s」并指派给你。", notice.Identifier, title)
	}
	if publicURL == "" {
		return line
	}
	return line + "\n" + publicURL + "/issues/" + notice.Identifier
}

// mentionNoticeText composes the agent-voiced mention notice. The
// issue reference is resolved from the DB; a failed lookup degrades to
// a reference-less notice. The comment body is flattened (internal
// mention:// markup does not render in Lark text messages) and
// truncated so a wall-of-text comment doesn't arrive as a wall-of-text
// DM.
func (n *AgentNotifier) mentionNoticeText(ctx context.Context, notice MentionNotice) string {
	var identifier, title string
	if issue, err := n.queries.GetIssue(ctx, notice.IssueID); err == nil {
		title = strings.TrimSpace(issue.Title)
		identifier = fmt.Sprintf("#%d", issue.Number)
		if ws, werr := n.queries.GetWorkspace(ctx, notice.WorkspaceID); werr == nil && ws.IssuePrefix != "" {
			identifier = fmt.Sprintf("%s-%d", ws.IssuePrefix, issue.Number)
		}
	}

	var head string
	switch {
	case identifier != "" && title != "":
		head = fmt.Sprintf("我在 %s「%s」的评论里提到了你：", identifier, title)
	case identifier != "":
		head = fmt.Sprintf("我在 %s 的评论里提到了你：", identifier)
	default:
		head = "我在一条评论里提到了你："
	}

	body := truncateRunes(flattenMentionMarkup(strings.TrimSpace(notice.CommentBody)), mentionBodyMaxRunes)
	text := head
	if body != "" {
		text += "\n" + body
	}
	if n.cfg.PublicURL != "" && identifier != "" {
		text += "\n" + n.cfg.PublicURL + "/issues/" + identifier
	}
	return text
}

// mentionBodyMaxRunes caps the quoted comment body inside a mention
// notice. Long enough to carry the ask, short enough that the DM stays
// a notification rather than a transplanted document.
const mentionBodyMaxRunes = 300

// truncateRunes shortens s to at most max runes, appending an ellipsis
// when something was cut. Rune-based so CJK text doesn't get split
// mid-character.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// flattenMentionMarkup rewrites Multica's internal mention links —
// `[@Name](mention://member/<id>)`, `[MUL-42](mention://issue/<id>)` —
// into plain text. Lark `msg_type=text` renders neither markdown links
// nor the mention:// scheme, so without this the raw markup leaks into
// the DM verbatim (observed live with `[SMO-5](mention://issue/…)`).
// Member/agent/squad/all mentions keep an @ prefix so the sentence
// still reads as a mention; issue references become their bare key.
func flattenMentionMarkup(s string) string {
	return util.MentionRe.ReplaceAllStringFunc(s, func(match string) string {
		groups := util.MentionRe.FindStringSubmatch(match)
		if len(groups) < 3 {
			return match
		}
		label := groups[1]
		if groups[2] == "issue" {
			return label
		}
		return "@" + label
	})
}

func (n *AgentNotifier) installationCredentials(inst db.LarkInstallation) (InstallationCredentials, error) {
	if n.credentials == nil {
		return InstallationCredentials{}, fmt.Errorf("lark assignment notifier: credentials resolver missing")
	}
	secret, err := n.credentials.DecryptAppSecret(inst)
	if err != nil {
		return InstallationCredentials{}, fmt.Errorf("decrypt app_secret: %w", err)
	}
	creds := InstallationCredentials{
		AppID:     inst.AppID,
		AppSecret: secret,
		Region:    RegionOrDefault(inst.Region),
	}
	if inst.TenantKey.Valid {
		creds.TenantKey = inst.TenantKey.String
	}
	return creds, nil
}
