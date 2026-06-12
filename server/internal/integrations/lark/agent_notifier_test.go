package lark

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeAgentNotifierQueries is the unit-test seam for AgentNotifierQueries.
type fakeAgentNotifierQueries struct {
	installation    db.LarkInstallation
	installationErr error
	binding         db.LarkUserBinding
	bindingErr      error
	issue           db.Issue
	issueErr        error
	workspace       db.Workspace
	workspaceErr    error
}

func (f *fakeAgentNotifierQueries) GetLarkInstallationByAgent(ctx context.Context, arg db.GetLarkInstallationByAgentParams) (db.LarkInstallation, error) {
	return f.installation, f.installationErr
}

func (f *fakeAgentNotifierQueries) GetLarkUserBindingByUser(ctx context.Context, arg db.GetLarkUserBindingByUserParams) (db.LarkUserBinding, error) {
	return f.binding, f.bindingErr
}

func (f *fakeAgentNotifierQueries) GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error) {
	return f.issue, f.issueErr
}

func (f *fakeAgentNotifierQueries) GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error) {
	return f.workspace, f.workspaceErr
}

// newTestAgentNotifier builds a notifier with inline dispatch so
// assertions run after NotifyAssigned returns.
func newTestAgentNotifier(q *fakeAgentNotifierQueries, api *fakeAPIClient, chat *fakeChat, publicURL string) *AgentNotifier {
	n := NewAgentNotifier(q, fakeCredentials{secret: "s"}, api, chat, AgentNotifierConfig{
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

// TestAgentNotifier_HappyPath pins the full flow: DM the bound
// assignee through the creating agent's Bot, then mirror the exact
// same text into the chat_session transcript via the chat_id Lark
// returned.
func TestAgentNotifier_HappyPath(t *testing.T) {
	sessionID := validUUID(0x66)
	q := &fakeAgentNotifierQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1", ChatID: ChatID("oc_p2p_9")}}
	chat := &fakeChat{ensureID: sessionID}
	n := newTestAgentNotifier(q, api, chat, "https://multica.test")

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

// TestAgentNotifier_NoInstallationSkips: an agent without a Lark
// Bot produces no delivery and no error noise.
func TestAgentNotifier_NoInstallationSkips(t *testing.T) {
	q := &fakeAgentNotifierQueries{installationErr: pgx.ErrNoRows}
	api := &fakeAPIClient{}
	chat := &fakeChat{}
	n := newTestAgentNotifier(q, api, chat, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 0 || chat.calledEnsure != 0 {
		t.Fatalf("no installation must produce no sends; dm=%d ensure=%d",
			len(api.userTextSent), chat.calledEnsure)
	}
}

// TestAgentNotifier_RevokedInstallationSkips mirrors the rest of
// the outbound surface: a revoked installation is not a delivery path.
func TestAgentNotifier_RevokedInstallationSkips(t *testing.T) {
	inst := activeInstallation()
	inst.Status = string(InstallationRevoked)
	q := &fakeAgentNotifierQueries{
		installation: inst,
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{}
	n := newTestAgentNotifier(q, api, &fakeChat{}, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 0 {
		t.Fatalf("revoked installation must produce no sends")
	}
}

// TestAgentNotifier_UnboundAssigneeSkips: an assignee without a
// lark_user_binding on this installation is silently skipped — the
// in-app inbox already has the notification.
func TestAgentNotifier_UnboundAssigneeSkips(t *testing.T) {
	q := &fakeAgentNotifierQueries{
		installation: activeInstallation(),
		bindingErr:   pgx.ErrNoRows,
	}
	api := &fakeAPIClient{}
	n := newTestAgentNotifier(q, api, &fakeChat{}, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 0 {
		t.Fatalf("unbound assignee must produce no sends")
	}
}

// TestAgentNotifier_MissingChatIDSkipsTranscript: an upstream
// response without chat_id delivers the DM but cannot mirror it; the
// transcript step is skipped without failing the delivery.
func TestAgentNotifier_MissingChatIDSkipsTranscript(t *testing.T) {
	q := &fakeAgentNotifierQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1"}}
	chat := &fakeChat{}
	n := newTestAgentNotifier(q, api, chat, "https://multica.test")

	n.NotifyAssigned(testNotice())

	if len(api.userTextSent) != 1 {
		t.Fatalf("DM should still be sent; got %d", len(api.userTextSent))
	}
	if chat.calledEnsure != 0 || len(chat.agentMessages) != 0 {
		t.Fatalf("missing chat_id must skip transcript mirroring; ensure=%d msgs=%d",
			chat.calledEnsure, len(chat.agentMessages))
	}
}

// TestAgentNotifier_TranscriptFailureDoesNotUndoDelivery: an
// EnsureChatSession / append failure is logged, not escalated — the
// user already has the message in Lark.
func TestAgentNotifier_TranscriptFailureDoesNotUndoDelivery(t *testing.T) {
	q := &fakeAgentNotifierQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_assignee"},
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1", ChatID: ChatID("oc_p2p_9")}}
	chat := &fakeChat{ensureErr: errors.New("db down")}
	n := newTestAgentNotifier(q, api, chat, "https://multica.test")

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

func testMentionNotice() MentionNotice {
	return MentionNotice{
		WorkspaceID:     validUUID(0x22),
		AgentID:         validUUID(0x33),
		MentionedUserID: validUUID(0x55),
		IssueID:         validUUID(0x77),
		CommentBody:     "[@bob](mention://member/55555555-5555-5555-5555-555555555555) 请处理 [SMO-6](mention://issue/77777777-7777-7777-7777-777777777777) 的回归测试。",
	}
}

// TestAgentNotifier_MentionHappyPath pins the comment-mention flow:
// DM the mentioned member through the commenting agent's Bot with the
// issue reference resolved from the DB, the comment body flattened
// (no raw mention:// markup), and the notice mirrored into the
// transcript.
func TestAgentNotifier_MentionHappyPath(t *testing.T) {
	sessionID := validUUID(0x66)
	q := &fakeAgentNotifierQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_mentioned"},
		issue:        db.Issue{Number: 6, Title: "新任务：测试流程"},
		workspace:    db.Workspace{IssuePrefix: "SMO"},
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1", ChatID: ChatID("oc_p2p_9")}}
	chat := &fakeChat{ensureID: sessionID}
	n := newTestAgentNotifier(q, api, chat, "https://multica.test")

	n.NotifyMentioned(testMentionNotice())

	if len(api.userTextSent) != 1 {
		t.Fatalf("expected one DM, got %d", len(api.userTextSent))
	}
	text := api.userTextSent[0].Text
	if api.userTextSent[0].OpenID != "ou_mentioned" {
		t.Errorf("open_id: got %q want ou_mentioned", api.userTextSent[0].OpenID)
	}
	if !strings.Contains(text, "SMO-6「新任务：测试流程」") {
		t.Errorf("text should carry the resolved issue reference; got %q", text)
	}
	if !strings.Contains(text, "@bob 请处理 SMO-6 的回归测试。") {
		t.Errorf("comment body should be flattened to plain text; got %q", text)
	}
	if strings.Contains(text, "mention://") {
		t.Errorf("raw mention markup must never reach Lark; got %q", text)
	}
	if !strings.Contains(text, "https://multica.test/issues/SMO-6") {
		t.Errorf("text should carry the deep link; got %q", text)
	}
	if len(chat.agentMessages) != 1 || chat.agentMessages[0] != text {
		t.Fatalf("transcript should mirror the DM text verbatim; got %+v", chat.agentMessages)
	}
}

// TestAgentNotifier_MentionIssueLookupDegrades: a failed issue lookup
// drops the reference and the link, never the notice.
func TestAgentNotifier_MentionIssueLookupDegrades(t *testing.T) {
	q := &fakeAgentNotifierQueries{
		installation: activeInstallation(),
		binding:      db.LarkUserBinding{LarkOpenID: "ou_mentioned"},
		issueErr:     pgx.ErrNoRows,
	}
	api := &fakeAPIClient{userTextReturn: SendUserTextResult{MessageID: "om_1", ChatID: ChatID("oc_p2p_9")}}
	n := newTestAgentNotifier(q, api, &fakeChat{}, "https://multica.test")

	n.NotifyMentioned(testMentionNotice())

	if len(api.userTextSent) != 1 {
		t.Fatalf("expected one DM, got %d", len(api.userTextSent))
	}
	text := api.userTextSent[0].Text
	if !strings.Contains(text, "我在一条评论里提到了你") {
		t.Errorf("degraded head expected; got %q", text)
	}
	if strings.Contains(text, "/issues/") {
		t.Errorf("failed issue lookup must omit the link; got %q", text)
	}
}

// TestAgentNotifier_MentionUnboundSkips mirrors the assignment path.
func TestAgentNotifier_MentionUnboundSkips(t *testing.T) {
	q := &fakeAgentNotifierQueries{
		installation: activeInstallation(),
		bindingErr:   pgx.ErrNoRows,
	}
	api := &fakeAPIClient{}
	n := newTestAgentNotifier(q, api, &fakeChat{}, "https://multica.test")

	n.NotifyMentioned(testMentionNotice())

	if len(api.userTextSent) != 0 {
		t.Fatalf("unbound mention target must produce no sends")
	}
}

func TestFlattenMentionMarkup(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[@bob](mention://member/1111-11) 看一下", "@bob 看一下"},
		{"关联 [SMO-5](mention://issue/2222-22)。", "关联 SMO-5。"},
		{"[@all](mention://all/all) 注意", "@all 注意"},
		{"普通文本不动", "普通文本不动"},
		{"[@PM](mention://agent/3333-33) 验收", "@PM 验收"},
	}
	for _, tc := range cases {
		if got := flattenMentionMarkup(tc.in); got != tc.want {
			t.Errorf("flatten(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("短文本", 300); got != "短文本" {
		t.Errorf("short text must pass through; got %q", got)
	}
	long := strings.Repeat("很", 400)
	got := truncateRunes(long, 300)
	if len([]rune(got)) != 301 || !strings.HasSuffix(got, "…") {
		t.Errorf("long text should cap at 300 runes + ellipsis; got %d runes", len([]rune(got)))
	}
}
