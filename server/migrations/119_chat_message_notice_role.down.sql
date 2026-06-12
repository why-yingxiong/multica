-- Notice rows cannot survive the narrowed CHECK; drop them first.
DELETE FROM chat_message WHERE role = 'notice';
ALTER TABLE chat_message DROP CONSTRAINT chat_message_role_check;
ALTER TABLE chat_message ADD CONSTRAINT chat_message_role_check
    CHECK (role IN ('user', 'assistant'));
