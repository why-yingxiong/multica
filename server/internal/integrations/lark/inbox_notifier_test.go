package lark

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeNotifierQueries is the unit-test seam for InboxNotifierQueries.
type fakeNotifierQueries struct {
	installations []db.LarkInstallation
	listErr       error
	// bindings keys on (installation_id bytes | user_id bytes).
	bindings   map[string]db.LarkUserBinding
	bindingErr error
	issue      db.Issue
	issueErr   error
	workspace  db.Workspace
	wsErr      error
}

func bindingKey(installationID, userID pgtype.UUID) string {
	return string(installationID.Bytes[:]) + "|" + string(userID.Bytes[:])
}

func (f *fakeNotifierQueries) ListLarkInstallationsByWorkspace(ctx context.Context, workspaceID pgtype.UUID) ([]db.LarkInstallation, error) {
	return f.installations, f.listErr
}

func (f *fakeNotifierQueries) GetLarkUserBindingByUser(ctx context.Context, arg db.GetLarkUserBindingByUserParams) (db.LarkUserBinding, error) {
	if f.bindingErr != nil {
		return db.LarkUserBinding{}, f.bindingErr
	}
	if b, ok := f.bindings[bindingKey(arg.InstallationID, arg.MulticaUserID)]; ok {
		return b, nil
	}
	return db.LarkUserBinding{}, pgx.ErrNoRows
}

func (f *fakeNotifierQueries) GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error) {
	return f.issue, f.issueErr
}

func (f *fakeNotifierQueries) GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error) {
	return f.workspace, f.wsErr
}

// newTestNotifier builds an InboxNotifier with inline dispatch so
// assertions run after handleEvent returns.
func newTestNotifier(q *fakeNotifierQueries, api *fakeAPIClient, publicURL string) *InboxNotifier {
	n := NewInboxNotifier(q, fakeCredentials{secret: "s"}, api, InboxNotifierConfig{
		PublicURL: publicURL,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	n.dispatch = func(fn func()) { fn() }
	return n
}

const (
	testWorkspaceIDStr = "22222222-2222-2222-2222-222222222222"
	testRecipientIDStr = "55555555-5555-5555-5555-555555555555"
)

func inboxEvent(item map[string]any) events.Event {
	return events.Event{
		Type:        protocol.EventInboxNew,
		WorkspaceID: testWorkspaceIDStr,
		Payload:     map[string]any{"item": item},
	}
}

func defaultInboxItem() map[string]any {
	body := "MUL-7 Fix login"
	issueID := "77777777-7777-7777-7777-777777777777"
	return map[string]any{
		"recipient_type": "member",
		"recipient_id":   testRecipientIDStr,
		"workspace_id":   testWorkspaceIDStr,
		"type":           "issue_assigned",
		"title":          "你被指派了一个任务",
		"body":           &body,
		"issue_id":       &issueID,
	}
}

func notifierInstallation(idByte byte) db.LarkInstallation {
	inst := activeInstallation()
	inst.ID = validUUID(idByte)
	return inst
}

func recipientUUID() pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(testRecipientIDStr)
	return u
}

// TestInboxNotifier_DMSendsToBoundRecipient pins DM mode: no notify
// group configured, recipient bound → one SendUserTextMessage to the
// recipient's open_id carrying title, body, and the deep link with the
// workspace-qualified issue key.
func TestInboxNotifier_DMSendsToBoundRecipient(t *testing.T) {
	inst := notifierInstallation(0x11)
	q := &fakeNotifierQueries{
		installations: []db.LarkInstallation{inst},
		bindings: map[string]db.LarkUserBinding{
			bindingKey(inst.ID, recipientUUID()): {LarkOpenID: "ou_recipient"},
		},
		issue:     db.Issue{Number: 7},
		workspace: db.Workspace{IssuePrefix: "MUL"},
	}
	api := &fakeAPIClient{userTextReturn: "om_1"}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(inboxEvent(defaultInboxItem()))

	if len(api.userTextSent) != 1 {
		t.Fatalf("expected one DM, got %d", len(api.userTextSent))
	}
	dm := api.userTextSent[0]
	if dm.OpenID != "ou_recipient" {
		t.Errorf("open_id: got %q want ou_recipient", dm.OpenID)
	}
	if !strings.Contains(dm.Text, "你被指派了一个任务") {
		t.Errorf("text should carry the title; got %q", dm.Text)
	}
	if !strings.Contains(dm.Text, "MUL-7 Fix login") {
		t.Errorf("text should carry the body; got %q", dm.Text)
	}
	if !strings.Contains(dm.Text, "https://multica.test/issues/MUL-7") {
		t.Errorf("text should carry the deep link; got %q", dm.Text)
	}
	if len(api.textSent) != 0 {
		t.Errorf("DM mode must not post to a chat; got %d", len(api.textSent))
	}
}

// TestInboxNotifier_GroupModeWinsAndMentionsRecipient pins group mode:
// an installation with notify_chat_id posts there with an <at> mention
// of the bound recipient, and NO DM goes out even though the recipient
// is bound.
func TestInboxNotifier_GroupModeWinsAndMentionsRecipient(t *testing.T) {
	inst := notifierInstallation(0x11)
	inst.NotifyChatID = pgtype.Text{String: "oc_notify_group", Valid: true}
	q := &fakeNotifierQueries{
		installations: []db.LarkInstallation{inst},
		bindings: map[string]db.LarkUserBinding{
			bindingKey(inst.ID, recipientUUID()): {LarkOpenID: "ou_recipient"},
		},
		issue:     db.Issue{Number: 7},
		workspace: db.Workspace{IssuePrefix: "MUL"},
	}
	api := &fakeAPIClient{textSendReturn: "om_2"}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(inboxEvent(defaultInboxItem()))

	if len(api.textSent) != 1 {
		t.Fatalf("expected one group message, got %d", len(api.textSent))
	}
	msg := api.textSent[0]
	if msg.ChatID != "oc_notify_group" {
		t.Errorf("chat_id: got %q want oc_notify_group", msg.ChatID)
	}
	if !strings.HasPrefix(msg.Text, `<at user_id="ou_recipient"></at> `) {
		t.Errorf("group message should lead with the <at> mention; got %q", msg.Text)
	}
	if len(api.userTextSent) != 0 {
		t.Errorf("group mode must suppress the DM; got %d DMs", len(api.userTextSent))
	}
}

// TestInboxNotifier_GroupModeUnboundRecipientStillPosts: the group
// post goes out without a mention when the recipient has no binding.
func TestInboxNotifier_GroupModeUnboundRecipientStillPosts(t *testing.T) {
	inst := notifierInstallation(0x11)
	inst.NotifyChatID = pgtype.Text{String: "oc_notify_group", Valid: true}
	q := &fakeNotifierQueries{
		installations: []db.LarkInstallation{inst},
		issue:         db.Issue{Number: 7},
		workspace:     db.Workspace{IssuePrefix: "MUL"},
	}
	api := &fakeAPIClient{textSendReturn: "om_3"}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(inboxEvent(defaultInboxItem()))

	if len(api.textSent) != 1 {
		t.Fatalf("expected one group message, got %d", len(api.textSent))
	}
	if strings.Contains(api.textSent[0].Text, "<at ") {
		t.Errorf("unbound recipient must not be mentioned; got %q", api.textSent[0].Text)
	}
}

// TestInboxNotifier_DMSkipsUnboundRecipient: DM mode with no binding
// is a silent no-op — the inbox item is already durable in-app.
func TestInboxNotifier_DMSkipsUnboundRecipient(t *testing.T) {
	q := &fakeNotifierQueries{
		installations: []db.LarkInstallation{notifierInstallation(0x11)},
	}
	api := &fakeAPIClient{}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(inboxEvent(defaultInboxItem()))

	if len(api.userTextSent) != 0 || len(api.textSent) != 0 {
		t.Fatalf("unbound recipient must produce no sends; dm=%d group=%d",
			len(api.userTextSent), len(api.textSent))
	}
}

// TestInboxNotifier_AgentRecipientIgnored: agent inboxes are consumed
// programmatically; no Lark delivery.
func TestInboxNotifier_AgentRecipientIgnored(t *testing.T) {
	q := &fakeNotifierQueries{
		installations: []db.LarkInstallation{notifierInstallation(0x11)},
	}
	api := &fakeAPIClient{}
	n := newTestNotifier(q, api, "https://multica.test")

	item := defaultInboxItem()
	item["recipient_type"] = "agent"
	n.handleEvent(inboxEvent(item))

	if len(api.userTextSent) != 0 || len(api.textSent) != 0 {
		t.Fatalf("agent recipient must produce no sends")
	}
}

// TestInboxNotifier_RevokedInstallationSkipped: a revoked installation
// is invisible to both modes.
func TestInboxNotifier_RevokedInstallationSkipped(t *testing.T) {
	inst := notifierInstallation(0x11)
	inst.Status = string(InstallationRevoked)
	inst.NotifyChatID = pgtype.Text{String: "oc_notify_group", Valid: true}
	q := &fakeNotifierQueries{
		installations: []db.LarkInstallation{inst},
		bindings: map[string]db.LarkUserBinding{
			bindingKey(inst.ID, recipientUUID()): {LarkOpenID: "ou_recipient"},
		},
	}
	api := &fakeAPIClient{}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(inboxEvent(defaultInboxItem()))

	if len(api.userTextSent) != 0 || len(api.textSent) != 0 {
		t.Fatalf("revoked installation must produce no sends")
	}
}

// TestInboxNotifier_NoInstallationsNoop: workspaces without Lark are
// unaffected.
func TestInboxNotifier_NoInstallationsNoop(t *testing.T) {
	q := &fakeNotifierQueries{}
	api := &fakeAPIClient{}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(inboxEvent(defaultInboxItem()))

	if len(api.userTextSent) != 0 || len(api.textSent) != 0 {
		t.Fatalf("no installations must produce no sends")
	}
}

// TestInboxNotifier_LinkDegradesWhenIssueLookupFails: a failed issue
// lookup (or missing public URL) drops the link, never the message.
func TestInboxNotifier_LinkDegradesWhenIssueLookupFails(t *testing.T) {
	inst := notifierInstallation(0x11)
	q := &fakeNotifierQueries{
		installations: []db.LarkInstallation{inst},
		bindings: map[string]db.LarkUserBinding{
			bindingKey(inst.ID, recipientUUID()): {LarkOpenID: "ou_recipient"},
		},
		issueErr: pgx.ErrNoRows,
	}
	api := &fakeAPIClient{userTextReturn: "om_4"}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(inboxEvent(defaultInboxItem()))

	if len(api.userTextSent) != 1 {
		t.Fatalf("expected one DM, got %d", len(api.userTextSent))
	}
	if strings.Contains(api.userTextSent[0].Text, "/issues/") {
		t.Errorf("failed issue lookup must omit the link; got %q", api.userTextSent[0].Text)
	}
	if !strings.Contains(api.userTextSent[0].Text, "你被指派了一个任务") {
		t.Errorf("title must survive the degraded path; got %q", api.userTextSent[0].Text)
	}
}

// TestInboxNotifier_MalformedPayloadIgnored: payloads that don't carry
// a parseable item are skipped without panicking the bus.
func TestInboxNotifier_MalformedPayloadIgnored(t *testing.T) {
	q := &fakeNotifierQueries{}
	api := &fakeAPIClient{}
	n := newTestNotifier(q, api, "https://multica.test")

	n.handleEvent(events.Event{Type: protocol.EventInboxNew, Payload: "not a map"})
	n.handleEvent(events.Event{Type: protocol.EventInboxNew, Payload: map[string]any{"item": 42}})
	n.handleEvent(events.Event{Type: protocol.EventInboxNew, Payload: map[string]any{
		"item": map[string]any{"recipient_type": "member"},
	}})

	if len(api.userTextSent) != 0 || len(api.textSent) != 0 {
		t.Fatalf("malformed payloads must produce no sends")
	}
}
