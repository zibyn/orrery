-- The first-User OIDC scenario needs a Control Plane whose users table is
-- empty (ADR-0020), so it gets its own database next to the main one.
CREATE DATABASE stars_oidc;
