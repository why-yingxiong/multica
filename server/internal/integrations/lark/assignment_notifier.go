package lark

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AssignmentNotifierQueries is the narrow subset of *db.Queries the
// AssignmentNotifier needs. *db.Queries satisfies it directly; tests
// substitute a fake.
type AssignmentNotifierQueries interface {
	GetLarkInstallationByAgent(ctx context.Context, arg db.GetLarkInstallationByAgentParams) (db.LarkInstallation, error)
	GetLarkUserBindingByUser(ctx context.Context, arg db.GetLarkUserBindingByUserParams) (db.LarkUserBinding, error)
}

// AssignmentNotifierConfig tunes the notifier.
type AssignmentNotifierConfig struct {
	// PublicURL is the Multica HTTP host used for the issue deep link
	// ("https://multica.example"). Empty omits the link.
	PublicURL string
	Logger    *slog.Logger
}

func (c AssignmentNotifierConfig) withDefaults() AssignmentNotifierConfig {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	return c
}

// assignmentNotifyTimeout bounds one notice delivery (two DB lookups,
// one Lark HTTP send, one transcript append). The work runs detached
// from the publishing goroutine.
const assignmentNotifyTimeout = 10 * time.Second

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

// AssignmentNotifier tells an issue's assignee — in conversation —
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
type AssignmentNotifier struct {
	queries     AssignmentNotifierQueries
	credentials CredentialsResolver
	client      APIClient
	chat        ChatSessionService
	cfg         AssignmentNotifierConfig

	// dispatch is the seam tests use to run deliveries inline. In
	// production it detaches a goroutine so issue creation never waits
	// on a Lark HTTP round-trip.
	dispatch func(func())
}

// NewAssignmentNotifier constructs an AssignmentNotifier bound to its
// dependencies.
func NewAssignmentNotifier(
	queries AssignmentNotifierQueries,
	credentials CredentialsResolver,
	client APIClient,
	chat ChatSessionService,
	cfg AssignmentNotifierConfig,
) *AssignmentNotifier {
	return &AssignmentNotifier{
		queries:     queries,
		credentials: credentials,
		client:      client,
		chat:        chat,
		cfg:         cfg.withDefaults(),
		dispatch:    func(fn func()) { go fn() },
	}
}

// NotifyAssigned delivers one notice asynchronously. Call sites pass
// already-validated facts (creator is an agent, assignee is a member);
// the notifier owns the Lark-side checks.
func (n *AssignmentNotifier) NotifyAssigned(notice IssueAssignedNotice) {
	n.dispatch(func() {
		ctx, cancel := context.WithTimeout(context.Background(), assignmentNotifyTimeout)
		defer cancel()
		if err := n.notify(ctx, notice); err != nil {
			n.cfg.Logger.Warn("lark assignment notifier: delivery failed",
				"workspace_id", uuidString(notice.WorkspaceID),
				"agent_id", uuidString(notice.AgentID),
				"assignee_user_id", uuidString(notice.AssigneeUserID),
				"issue", notice.Identifier,
				"error", err,
			)
		}
	})
}

func (n *AssignmentNotifier) notify(ctx context.Context, notice IssueAssignedNotice) error {
	inst, err := n.queries.GetLarkInstallationByAgent(ctx, db.GetLarkInstallationByAgentParams{
		WorkspaceID: notice.WorkspaceID,
		AgentID:     notice.AgentID,
	})
	if err != nil {
		if isNoRowsErr(err) {
			// The creating agent has no Lark Bot — nothing to deliver
			// through. Not an error.
			return nil
		}
		return fmt.Errorf("load installation: %w", err)
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		return nil
	}

	binding, err := n.queries.GetLarkUserBindingByUser(ctx, db.GetLarkUserBindingByUserParams{
		InstallationID: inst.ID,
		MulticaUserID:  notice.AssigneeUserID,
	})
	if err != nil {
		if isNoRowsErr(err) {
			n.cfg.Logger.Debug("lark assignment notifier: assignee not bound; skipped",
				"installation_id", uuidString(inst.ID),
				"assignee_user_id", uuidString(notice.AssigneeUserID),
			)
			return nil
		}
		return fmt.Errorf("load assignee binding: %w", err)
	}

	creds, err := n.installationCredentials(inst)
	if err != nil {
		return err
	}

	text := assignmentNoticeText(notice, n.cfg.PublicURL)
	res, err := n.client.SendUserTextMessage(ctx, SendUserTextParams{
		InstallationID: creds,
		OpenID:         OpenID(binding.LarkOpenID),
		Text:           text,
	})
	if err != nil {
		return fmt.Errorf("send assignment notice: %w", err)
	}

	// Mirror the notice into the conversation transcript so it is part
	// of the agent's context on the next run. Best-effort: the user
	// already has the message in Lark; a transcript miss only costs
	// context, never the notification itself.
	if res.ChatID == "" {
		n.cfg.Logger.Warn("lark assignment notifier: send response missing chat_id; notice not mirrored to transcript",
			"installation_id", uuidString(inst.ID),
			"issue", notice.Identifier,
		)
		return nil
	}
	sessionID, err := n.chat.EnsureChatSession(ctx, EnsureChatSessionParams{
		WorkspaceID:    inst.WorkspaceID,
		InstallationID: inst.ID,
		AgentID:        inst.AgentID,
		ChatID:         res.ChatID,
		ChatType:       ChatTypeP2P,
		Sender:         notice.AssigneeUserID,
	})
	if err != nil {
		n.cfg.Logger.Warn("lark assignment notifier: ensure chat session failed; notice not mirrored to transcript",
			"installation_id", uuidString(inst.ID),
			"chat_id", string(res.ChatID),
			"error", err,
		)
		return nil
	}
	if err := n.chat.AppendAgentMessage(ctx, sessionID, text); err != nil {
		n.cfg.Logger.Warn("lark assignment notifier: transcript append failed",
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

func (n *AssignmentNotifier) installationCredentials(inst db.LarkInstallation) (InstallationCredentials, error) {
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
