INSERT INTO widgets (id, name, active) VALUES (1, 'Sample widget', true)
ON CONFLICT (id) DO NOTHING;
