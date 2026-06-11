package lark

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// InboxNotifierQueries is the narrow subset of *db.Queries the
// InboxNotifier needs. *db.Queries satisfies it directly; tests
// substitute a fake.
type InboxNotifierQueries interface {
	ListLarkInstallationsByWorkspace(ctx context.Context, workspaceID pgtype.UUID) ([]db.LarkInstallation, error)
	GetLarkUserBindingByUser(ctx context.Context, arg db.GetLarkUserBindingByUserParams) (db.LarkUserBinding, error)
	GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error)
	GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error)
}

// InboxNotifierConfig tunes the notifier. Defaults via withDefaults.
type InboxNotifierConfig struct {
	// PublicURL is the Multica HTTP host used for the issue deep link
	// ("https://multica.example"). Empty omits the link — the
	// notification still carries the title/body.
	PublicURL string
	Logger    *slog.Logger
}

func (c InboxNotifierConfig) withDefaults() InboxNotifierConfig {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	return c
}

// inboxNotifyTimeout bounds one notification delivery (DB lookups + a
// single Lark HTTP send). The work runs detached from the publishing
// goroutine, so this only caps resource usage, not request latency.
const inboxNotifyTimeout = 10 * time.Second

// InboxNotifier forwards inbox items (the in-app notification feed) to
// Lark. It is the bridge the integration's MVP deliberately left out:
// EventInboxNew → at most ONE Lark message per item, routed by the
// workspace's installation config:
//
//   - Group mode — the first active installation with notify_chat_id
//     set (configured via `/notify on` in a group) receives the item
//     as a group message, prefixed with an <at> mention when the
//     recipient has a lark_user_binding on that installation.
//
//   - DM mode — otherwise, the first active installation where the
//     recipient has a binding DMs them via their open_id.
//
//   - No active installation, or no binding in DM mode → silently
//     skipped (debug log). The inbox item itself is already durable;
//     Lark delivery is strictly best-effort, mirroring the Patcher.
//
// Group mode wins over DM mode so a team that opted into a shared
// notification channel doesn't ALSO get DM'd for every item.
//
// Multi-replica safety is the same argument as the Patcher's: the bus
// is per-process and the inbox item is created (and its event
// published) on exactly one replica, so exactly one notifier reacts.
type InboxNotifier struct {
	queries     InboxNotifierQueries
	credentials CredentialsResolver
	client      APIClient
	cfg         InboxNotifierConfig

	// dispatch is the seam tests use to run deliveries inline. In
	// production it detaches a goroutine so a burst of inbox events
	// (one per subscriber) never serializes Lark HTTP round-trips into
	// the publishing request's critical path.
	dispatch func(func())
}

// NewInboxNotifier constructs an InboxNotifier bound to its
// dependencies. It does not subscribe to the bus until Register is
// called.
func NewInboxNotifier(queries InboxNotifierQueries, credentials CredentialsResolver, client APIClient, cfg InboxNotifierConfig) *InboxNotifier {
	return &InboxNotifier{
		queries:     queries,
		credentials: credentials,
		client:      client,
		cfg:         cfg.withDefaults(),
		dispatch:    func(fn func()) { go fn() },
	}
}

// Register subscribes the notifier to EventInboxNew. Call exactly once
// during server boot, after the bus and notifier are constructed.
func (n *InboxNotifier) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventInboxNew, n.handleEvent)
}

// inboxEventItem is the slice of the EventInboxNew payload the
// notifier consumes. The payload is the same map the WS layer
// broadcasts (inboxItemToResponse in cmd/server), parsed defensively —
// a malformed field skips the event rather than panicking the bus.
type inboxEventItem struct {
	RecipientType string
	RecipientID   string
	WorkspaceID   string
	Title         string
	Body          string
	IssueID       string
}

func (n *InboxNotifier) handleEvent(e events.Event) {
	item, ok := inboxItemFromEvent(e)
	if !ok {
		return
	}
	// Only members have a Lark identity to deliver to; agent
	// recipients consume their inbox programmatically.
	if item.RecipientType != "member" {
		return
	}
	n.dispatch(func() {
		ctx, cancel := context.WithTimeout(context.Background(), inboxNotifyTimeout)
		defer cancel()
		if err := n.notify(ctx, item); err != nil {
			n.cfg.Logger.Warn("lark inbox notifier: delivery failed",
				"workspace_id", item.WorkspaceID,
				"recipient_id", item.RecipientID,
				"error", err,
			)
		}
	})
}

// notify routes one inbox item per the group-over-DM rule and sends at
// most one Lark message.
func (n *InboxNotifier) notify(ctx context.Context, item inboxEventItem) error {
	var workspaceID, recipientID pgtype.UUID
	if err := workspaceID.Scan(item.WorkspaceID); err != nil {
		return fmt.Errorf("parse workspace_id: %w", err)
	}
	if err := recipientID.Scan(item.RecipientID); err != nil {
		return fmt.Errorf("parse recipient_id: %w", err)
	}

	installations, err := n.queries.ListLarkInstallationsByWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("list installations: %w", err)
	}
	active := installations[:0]
	for _, inst := range installations {
		if InstallationStatus(inst.Status) == InstallationActive {
			active = append(active, inst)
		}
	}
	if len(active) == 0 {
		return nil
	}

	text := n.messageText(ctx, item, workspaceID)

	// Group mode: the first active installation with a configured
	// notification group wins for the whole workspace.
	for _, inst := range active {
		if !inst.NotifyChatID.Valid || inst.NotifyChatID.String == "" {
			continue
		}
		return n.sendGroup(ctx, inst, recipientID, text)
	}

	// DM mode: the first active installation where the recipient is
	// bound delivers; an unbound recipient is a silent skip.
	for _, inst := range active {
		binding, err := n.queries.GetLarkUserBindingByUser(ctx, db.GetLarkUserBindingByUserParams{
			InstallationID: inst.ID,
			MulticaUserID:  recipientID,
		})
		if err != nil {
			if isNoRowsErr(err) {
				continue
			}
			return fmt.Errorf("lookup recipient binding: %w", err)
		}
		creds, err := n.installationCredentials(inst)
		if err != nil {
			return err
		}
		if _, err := n.client.SendUserTextMessage(ctx, SendUserTextParams{
			InstallationID: creds,
			OpenID:         OpenID(binding.LarkOpenID),
			Text:           text,
		}); err != nil {
			return fmt.Errorf("send dm: %w", err)
		}
		return nil
	}

	n.cfg.Logger.Debug("lark inbox notifier: recipient has no binding; skipped",
		"workspace_id", item.WorkspaceID,
		"recipient_id", item.RecipientID,
	)
	return nil
}

// sendGroup posts the item into the installation's notification group,
// <at>-mentioning the recipient when they are bound on this
// installation (an unbound recipient still gets the group post — the
// content is workspace-visible by the act of configuring the group).
func (n *InboxNotifier) sendGroup(ctx context.Context, inst db.LarkInstallation, recipientID pgtype.UUID, text string) error {
	binding, err := n.queries.GetLarkUserBindingByUser(ctx, db.GetLarkUserBindingByUserParams{
		InstallationID: inst.ID,
		MulticaUserID:  recipientID,
	})
	switch {
	case err == nil && binding.LarkOpenID != "":
		// Lark renders <at user_id="ou_..."></at> in text messages as a
		// real mention that pings the user.
		text = fmt.Sprintf("<at user_id=%q></at> %s", binding.LarkOpenID, text)
	case err != nil && !isNoRowsErr(err):
		return fmt.Errorf("lookup recipient binding: %w", err)
	}
	creds, err := n.installationCredentials(inst)
	if err != nil {
		return err
	}
	if _, err := n.client.SendTextMessage(ctx, SendTextParams{
		InstallationID: creds,
		ChatID:         ChatID(inst.NotifyChatID.String),
		Text:           text,
	}); err != nil {
		return fmt.Errorf("send group message: %w", err)
	}
	return nil
}

// messageText renders the notification body: title, optional body
// line, optional issue deep link. The link needs two extra lookups
// (issue number + workspace prefix); either failing degrades to a
// link-less message rather than dropping the notification.
func (n *InboxNotifier) messageText(ctx context.Context, item inboxEventItem, workspaceID pgtype.UUID) string {
	var b strings.Builder
	b.WriteString(item.Title)
	if body := strings.TrimSpace(item.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
	}
	if link := n.issueLink(ctx, item.IssueID, workspaceID); link != "" {
		b.WriteString("\n")
		b.WriteString(link)
	}
	return b.String()
}

func (n *InboxNotifier) issueLink(ctx context.Context, issueID string, workspaceID pgtype.UUID) string {
	if n.cfg.PublicURL == "" || issueID == "" {
		return ""
	}
	var id pgtype.UUID
	if err := id.Scan(issueID); err != nil {
		return ""
	}
	issue, err := n.queries.GetIssue(ctx, id)
	if err != nil {
		return ""
	}
	identifier := fmt.Sprintf("#%d", issue.Number)
	if ws, err := n.queries.GetWorkspace(ctx, workspaceID); err == nil && ws.IssuePrefix != "" {
		identifier = fmt.Sprintf("%s-%d", ws.IssuePrefix, issue.Number)
	}
	return n.cfg.PublicURL + "/issues/" + identifier
}

func (n *InboxNotifier) installationCredentials(inst db.LarkInstallation) (InstallationCredentials, error) {
	if n.credentials == nil {
		return InstallationCredentials{}, fmt.Errorf("lark inbox notifier: credentials resolver missing")
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

// inboxItemFromEvent extracts the fields the notifier needs from the
// EventInboxNew payload. Every field is optional-chained; a payload
// that doesn't carry a parseable item returns ok=false.
func inboxItemFromEvent(e events.Event) (inboxEventItem, bool) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return inboxEventItem{}, false
	}
	raw, ok := m["item"].(map[string]any)
	if !ok {
		return inboxEventItem{}, false
	}
	item := inboxEventItem{
		RecipientType: stringField(raw, "recipient_type"),
		RecipientID:   stringField(raw, "recipient_id"),
		WorkspaceID:   stringField(raw, "workspace_id"),
		Title:         stringField(raw, "title"),
		Body:          stringField(raw, "body"),
		IssueID:       stringField(raw, "issue_id"),
	}
	if item.WorkspaceID == "" {
		item.WorkspaceID = e.WorkspaceID
	}
	if item.RecipientID == "" || item.WorkspaceID == "" {
		return inboxEventItem{}, false
	}
	return item, true
}

// stringField reads a string-ish payload field: plain strings and
// *string (the inbox response uses pointers for nullable columns).
func stringField(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case *string:
		if v != nil {
			return *v
		}
	}
	return ""
}
