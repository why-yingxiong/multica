-- Inbox → Lark notification routing (group mode). When set, inbox
-- notifications for this installation's workspace are posted into this
-- Lark group chat (with an <at> mention when the recipient has a
-- lark_user_binding) instead of DM'd to the recipient's open_id.
-- Configured via the `/notify on|off` command sent to the Bot in a
-- group chat; NULL means "DM mode" (the default).
ALTER TABLE lark_installation ADD COLUMN notify_chat_id TEXT;
