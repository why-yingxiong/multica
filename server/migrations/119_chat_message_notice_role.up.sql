-- Bot-initiated notices in chat transcripts. The Lark AgentNotifier
-- mirrors platform-composed messages ("我创建了 SMO-8 并指派给你") into
-- the chat_session so the agent keeps its own notifications in
-- conversation context. Those rows must NOT be 'assistant':
--
--   1. The daemon chat prompt anchors on the last 'assistant' row and
--      replays everything after it. A platform notice posing as
--      'assistant' would advance that anchor and silently swallow any
--      user message that arrived before the notice landed.
--   2. The provider session (--resume) only contains text the model
--      actually generated; a notice needs to be re-delivered through
--      the next prompt, which requires telling it apart from real
--      assistant turns.
--
-- 'notice' rows are included in the next run's prompt (wrapped in a
-- system marker) and render like assistant messages in the UI.
ALTER TABLE chat_message DROP CONSTRAINT chat_message_role_check;
ALTER TABLE chat_message ADD CONSTRAINT chat_message_role_check
    CHECK (role IN ('user', 'assistant', 'notice'));
