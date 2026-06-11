package lark

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// errBoom is the generic infra failure used by the /notify config-write
// failure test.
var errBoom = errors.New("boom")

func TestParseNotifyCommand(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		action NotifyAction
		ok     bool
	}{
		{"on", "/notify on", NotifyActionOn, true},
		{"off", "/notify off", NotifyActionOff, true},
		{"on uppercase arg", "/notify ON", NotifyActionOn, true},
		{"bare", "/notify", NotifyActionHelp, true},
		{"unknown arg", "/notify maybe", NotifyActionHelp, true},
		{"leading blank lines", "\n\n  /notify on", NotifyActionOn, true},
		{"trailing whitespace", "/notify off  \t", NotifyActionOff, true},
		{"prefix of longer token", "/notifyme on", "", false},
		{"case-sensitive prefix", "/Notify on", "", false},
		{"inline mention", "please run /notify on", "", false},
		{"plain chat", "hello there", "", false},
		{"empty", "", "", false},
		{"second line ignored", "deploy it\n/notify on", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, ok := parseNotifyCommand(tc.body)
			if ok != tc.ok {
				t.Fatalf("ok: got %v want %v", ok, tc.ok)
			}
			if action != tc.action {
				t.Fatalf("action: got %q want %q", action, tc.action)
			}
		})
	}
}

// TestDispatcher_NotifyOnInGroupSetsNotifyChat pins the happy path:
// `/notify on` from a bound user in a group writes notify_chat_id,
// does NOT touch chat_session (the command is config, not
// conversation), does NOT enqueue an agent run, and finalizes the
// dedup claim as processed.
func TestDispatcher_NotifyOnInGroupSetsNotifyChat(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
	}
	chat := &fakeChat{}
	enq := &fakeEnqueuer{}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        chat,
		Audit:       &fakeAudit{},
		TaskService: enq,
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatID:         ChatID("oc_group_1"),
		ChatType:       ChatTypeGroup,
		AddressedToBot: true,
		SenderOpenID:   "ou_user_a",
		Body:           "/notify on",
		MessageID:      "msg-notify-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeNotifyCommand {
		t.Fatalf("outcome: got %q want %q", res.Outcome, OutcomeNotifyCommand)
	}
	if res.NotifyResult != NotifyResultEnabled {
		t.Fatalf("notify result: got %q want %q", res.NotifyResult, NotifyResultEnabled)
	}
	if len(queries.notifyChatSet) != 1 {
		t.Fatalf("expected one SetLarkInstallationNotifyChat call, got %d", len(queries.notifyChatSet))
	}
	set := queries.notifyChatSet[0]
	if !set.NotifyChatID.Valid || set.NotifyChatID.String != "oc_group_1" {
		t.Fatalf("notify_chat_id: got %+v want oc_group_1", set.NotifyChatID)
	}
	if chat.calledEnsure != 0 || chat.calledAppend != 0 {
		t.Fatalf("config command must not touch chat_session; ensure=%d append=%d",
			chat.calledEnsure, chat.calledAppend)
	}
	if enq.called != 0 {
		t.Fatalf("config command must not enqueue an agent run; called=%d", enq.called)
	}
	row := queries.dedup[seedDedupKey("msg-notify-1")]
	if row == nil || !row.processed {
		t.Fatalf("dedup claim should be marked processed; row=%+v", row)
	}
}

// TestDispatcher_NotifyOnInP2PRejected: the notify group only makes
// sense for group chats; p2p delivery is the default and needs no
// opt-in. The command is acknowledged with group_only copy and no
// config write happens.
func TestDispatcher_NotifyOnInP2PRejected(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
	}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        &fakeChat{},
		Audit:       &fakeAudit{},
		TaskService: &fakeEnqueuer{},
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatID:       ChatID("oc_p2p_1"),
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "/notify on",
		MessageID:    "msg-notify-2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.NotifyResult != NotifyResultGroupOnly {
		t.Fatalf("notify result: got %q want %q", res.NotifyResult, NotifyResultGroupOnly)
	}
	if len(queries.notifyChatSet) != 0 {
		t.Fatalf("p2p /notify on must not write config; got %d calls", len(queries.notifyChatSet))
	}
}

// TestDispatcher_NotifyOffClearsNotifyChat: `/notify off` clears the
// group unconditionally — including from p2p, so an admin can disable
// routing without finding the configured group.
func TestDispatcher_NotifyOffClearsNotifyChat(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
	}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        &fakeChat{},
		Audit:       &fakeAudit{},
		TaskService: &fakeEnqueuer{},
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:        "ok",
		ChatID:       ChatID("oc_p2p_1"),
		ChatType:     ChatTypeP2P,
		SenderOpenID: "ou_user_a",
		Body:         "/notify off",
		MessageID:    "msg-notify-3",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.NotifyResult != NotifyResultDisabled {
		t.Fatalf("notify result: got %q want %q", res.NotifyResult, NotifyResultDisabled)
	}
	if len(queries.notifyChatSet) != 1 {
		t.Fatalf("expected one SetLarkInstallationNotifyChat call, got %d", len(queries.notifyChatSet))
	}
	if queries.notifyChatSet[0].NotifyChatID.Valid {
		t.Fatalf("off must clear (NULL) notify_chat_id; got %+v", queries.notifyChatSet[0].NotifyChatID)
	}
}

// TestDispatcher_NotifyHelpOnUnknownArg: `/notify whatever` answers
// with usage copy and never writes config.
func TestDispatcher_NotifyHelpOnUnknownArg(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
	}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        &fakeChat{},
		Audit:       &fakeAudit{},
		TaskService: &fakeEnqueuer{},
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatID:         ChatID("oc_group_1"),
		ChatType:       ChatTypeGroup,
		AddressedToBot: true,
		SenderOpenID:   "ou_user_a",
		Body:           "/notify pls",
		MessageID:      "msg-notify-4",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.NotifyResult != NotifyResultHelp {
		t.Fatalf("notify result: got %q want %q", res.NotifyResult, NotifyResultHelp)
	}
	if len(queries.notifyChatSet) != 0 {
		t.Fatalf("help must not write config; got %d calls", len(queries.notifyChatSet))
	}
}

// TestDispatcher_NotifyFromUnboundUserAsksForBinding: identity check
// runs BEFORE the command — an unbound sender gets the binding prompt,
// and no config is written.
func TestDispatcher_NotifyFromUnboundUserAsksForBinding(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBindingErr:    pgx.ErrNoRows,
	}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        &fakeChat{},
		Audit:       &fakeAudit{},
		TaskService: &fakeEnqueuer{},
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatID:         ChatID("oc_group_1"),
		ChatType:       ChatTypeGroup,
		AddressedToBot: true,
		SenderOpenID:   "ou_stranger",
		Body:           "/notify on",
		MessageID:      "msg-notify-5",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeNeedsBinding {
		t.Fatalf("outcome: got %q want %q", res.Outcome, OutcomeNeedsBinding)
	}
	if len(queries.notifyChatSet) != 0 {
		t.Fatalf("unbound user must not write config; got %d calls", len(queries.notifyChatSet))
	}
}

// TestDispatcher_NotifyConfigWriteFailureReleasesClaim: a DB error on
// the config write is an infra failure with no durable outcome — the
// dedup claim must be released so the WS retry can re-run the command.
func TestDispatcher_NotifyConfigWriteFailureReleasesClaim(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
		notifyChatSetErr:  errBoom,
	}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        &fakeChat{},
		Audit:       &fakeAudit{},
		TaskService: &fakeEnqueuer{},
	}

	_, err := d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatID:         ChatID("oc_group_1"),
		ChatType:       ChatTypeGroup,
		AddressedToBot: true,
		SenderOpenID:   "ou_user_a",
		Body:           "/notify on",
		MessageID:      "msg-notify-6",
	})
	if err == nil {
		t.Fatalf("expected infra error to surface")
	}
	if queries.calledRelease != 1 {
		t.Fatalf("expected dedup claim release on infra failure; calledRelease=%d", queries.calledRelease)
	}
	if _, exists := queries.dedup[seedDedupKey("msg-notify-6")]; exists {
		t.Fatalf("released claim row should be deleted from dedup state")
	}
}

// TestDispatcher_CommandBodyWinsOverEnrichedBody: the enricher
// prepends quoted context to Body but leaves CommandBody untouched —
// `/notify` must parse from CommandBody, same contract as /issue.
func TestDispatcher_CommandBodyWinsOverEnrichedBody(t *testing.T) {
	queries := &fakeQueries{
		installationByApp: activeInstallation(),
		userBinding:       boundUser(),
	}
	d := &Dispatcher{
		Queries:     queries,
		Chat:        &fakeChat{},
		Audit:       &fakeAudit{},
		TaskService: &fakeEnqueuer{},
	}

	res, err := d.Handle(context.Background(), InboundMessage{
		AppID:          "ok",
		ChatID:         ChatID("oc_group_1"),
		ChatType:       ChatTypeGroup,
		AddressedToBot: true,
		SenderOpenID:   "ou_user_a",
		Body:           "<quoted_message>earlier text</quoted_message>\n/notify on",
		CommandBody:    "/notify on",
		MessageID:      "msg-notify-7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeNotifyCommand {
		t.Fatalf("outcome: got %q want %q (CommandBody should drive the parse)", res.Outcome, OutcomeNotifyCommand)
	}
}
