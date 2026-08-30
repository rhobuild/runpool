-- Operator pagination reads ready attempts across bindings. The scheduling
-- index begins with binding_id and cannot provide that global FIFO order.
CREATE INDEX attempts_ready_global
ON assignment_attempts (received_at, id)
WHERE state = 'ready';
