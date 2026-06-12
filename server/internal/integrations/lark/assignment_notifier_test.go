package lark

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeAssignmentQueries is the unit-test seam for AssignmentNotifierQueries.
type fakeAssignmentQueries struct {
	installation    db.LarkInstallation
	installationErr error
	binding         db.LarkUserBinding
	bindingErr      error
}

func (f *fakeAssignmentQueries) GetLarkInstallationByAgent(ctx context.Context, arg db.GetLarkInstallationByAgentParams) (db.LarkInstallation, error) {
	return f.installation, f.installationErr
}

func (f *fakeAssignmentQueries) GetLarkUserBindingByUser(ctx context.Context, arg db.GetLarkUserBindingByUserParams) (db.LarkUserBinding, error) {
	return f.binding, f.bindingErr
}

// newTestAssignmentNotifier builds a notifier with inline dispatch so
// assertions run after NotifyAssigned returns.
func newTestAssignmentNotifier(q *fakeAssignmentQueries, api *fakeAPIClient, chat *fakeChat, publicURL string) *AssignmentNotifier {
	n := NewAssignmentNotifier(q, fakeCredentials{secret: "s"}, api, chat, AssignmentNotifierConfig{
		PublicURL: publicURL,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	n.dispatch = func(fn func()) { fn() }
	return n
}

func testNotice() IssueAssignedNotice {
	return IssueAssignedNotice{
		WorkspaceID:    validUUID(0x22),
		AgentID:        validUUID(0x33),
		AssigneeUserID: validUUID(0x55),
		Identifier:     "SMO-6",
		Title:          "新任务：测试流程",
	}
}

// TestAssignmentNotifier_HappyPath pins the full flow: DM the bound
// assignee through the creating agent's Bot, then mirror the exact
// same text into the chat_session transcript via the chat_id Lark
// returned.
func TestAssignmentNotifier_HappyPath(t *testing.T) {
	sessionID := validUUID(0x66)
	q := &fakeAssignmentQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1", ChatID: ChatID("oc_p2p_9")}}
	chat := &fakeChat{ensureID: sessionID}
	n := newTestAssignmentNotifier(q, api, chat, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 1 {
		t.Fatalf("expected one DM, got %d", len(api.userTextSent))
	}
	dm := api.userTextSent[0]
	if dm.OpenID != "ou_assignee" {
		t.Errorf("open_id: got %q want ou_assignee", dm.OpenID)
	}
	if !strings.Contains(dm.Text, "SMO-6") || !strings.Contains(dm.Text, "新任务：测试流程") {
		t.Errorf("text should carry identifier and title; got %q", dm.Text)
	}
	if !strings.Contains(dm.Text, "https://multica.test/issues/SMO-6") {
		t.Errorf("text should carry the deep link; got %q", dm.Text)
	}

	// Transcript mirroring: session ensured against the chat_id from
	// the send response, notice appended verbatim as assistant message.
	if chat.calledEnsure != 1 {
		t.Fatalf("expected EnsureChatSession once, got %d", chat.calledEnsure)
	}
	if chat.lastEnsureParams.ChatID != ChatID("oc_p2p_9") {
		t.Errorf("ensure chat_id: got %q want oc_p2p_9", chat.lastEnsureParams.ChatID)
	}
	if chat.lastEnsureParams.ChatType != ChatTypeP2P {
		t.Errorf("ensure chat_type: got %q want p2p", chat.lastEnsureParams.ChatType)
	}
	if chat.lastEnsureParams.Sender != testNotice().AssigneeUserID {
		t.Errorf("session creator should be the assignee; got %+v", chat.lastEnsureParams.Sender)
	}
	if len(chat.agentMessages) != 1 || chat.agentMessages[0] != dm.Text {
		t.Fatalf("transcript should mirror the DM text verbatim; got %+v", chat.agentMessages)
	}
	if chat.lastAgentSession != sessionID {
		t.Errorf("append session: got %+v want %+v", chat.lastAgentSession, sessionID)
	}
}

// TestAssignmentNotifier_NoInstallationSkips: an agent without a Lark
// Bot produces no delivery and no error noise.
func TestAssignmentNotifier_NoInstallationSkips(t *testing.T) {
	q := &fakeAssignmentQueries{installationErr: pgx.ErrNoRows}
	api := &fakeAPIClient{}
	chat := &fakeChat{}
	n := newTestAssignmentNotifier(q, api, chat, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 0 || chat.calledEnsure != 0 {
		t.Fatalf("no installation must produce no sends; dm=%d ensure=%d",
			len(api.userTextSent), chat.calledEnsure)
	}
}

// TestAssignmentNotifier_RevokedInstallationSkips mirrors the rest of
// the outbound surface: a revoked installation is not a delivery path.
func TestAssignmentNotifier_RevokedInstallationSkips(t *testing.T) {
	inst := activeInstallation()
	inst.Status = string(InstallationRevoked)
	q := &fakeAssignmentQueries{
		installation: inst,
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{}
	n := newTestAssignmentNotifier(q, api, &fakeChat{}, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 0 {
		t.Fatalf("revoked installation must produce no sends")
	}
}

// TestAssignmentNotifier_UnboundAssigneeSkips: an assignee without a
// lark_user_binding on this installation is silently skipped — the
// in-app inbox already has the notification.
func TestAssignmentNotifier_UnboundAssigneeSkips(t *testing.T) {
	q := &fakeAssignmentQueries{
		installation: activeInstallation(),
		bindingErr:   pgx.ErrNoRows,
	}
	api := &fakeAPIClient{}
	n := newTestAssignmentNotifier(q, api, &fakeChat{}, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 0 {
		t.Fatalf("unbound assignee must produce no sends")
	}
}

// TestAssignmentNotifier_MissingChatIDSkipsTranscript: an upstream
// response without chat_id delivers the DM but cannot mirror it; the
// transcript step is skipped without failing the delivery.
func TestAssignmentNotifier_MissingChatIDSkipsTranscript(t *testing.T) {
	q := &fakeAssignmentQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1"}}
	chat := &fakeChat{}
	n := newTestAssignmentNotifier(q, api, chat, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 1 {
		t.Fatalf("DM should still be sent; got %d", len(api.userTextSent))
	}
	if chat.calledEnsure != 0 || len(chat.agentMessages) != 0 {
		t.Fatalf("missing chat_id must skip transcript mirroring; ensure=%d msgs=%d",
			chat.calledEnsure, len(chat.agentMessages))
	}
}

// TestAssignmentNotifier_TranscriptFailureDoesNotUndoDelivery: an
// EnsureChatSession / append failure is logged, not escalated — the
// user already has the message in Lark.
func TestAssignmentNotifier_TranscriptFailureDoesNotUndoDelivery(t *testing.T) {
	q := &fakeAssignmentQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1", ChatID: ChatID("oc_p2p_9")}}
	chat := &fakeChat{ensureErr: errors.New("db down")}
	n := newTestAssignmentNotifier(q, api, chat, "https://multica.test")

	// Must not panic and must not retract the DM (nothing to assert on
	// the Lark side beyond the send having happened).
	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 1 {
		t.Fatalf("DM should still be sent; got %d", len(api.userTextSent))
	}
}

// TestAssignmentNoticeText_NoPublicURL degrades to a link-less notice.
func TestAssignmentNoticeText_NoPublicURL(t *testing.T) {
	text := assignmentNoticeText(testNotice(), "")
	if strings.Contains(text, "/issues/") {
		t.Errorf("no public URL must omit the link; got %q", text)
	}
	if !strings.Contains(text, "SMO-6") {
		t.Errorf("identifier must survive; got %q", text)
	}
}
