-- Symmetric teardown. The link table goes first because it references secrets.
DROP TABLE IF EXISTS repo_origin_secrets;
DROP TABLE IF EXISTS secrets;
