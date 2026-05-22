ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_layer_check;

ALTER TABLE memories
    ADD CONSTRAINT memories_layer_check
    CHECK (layer IN ('long_term', 'daily', 'dreams', 'agent', 'analysis'));
