# Source this to get an isolated Claude Code and Codex home for one test user
# on the disposable k3d cluster. Your real ~/.claude and ~/.codex are untouched.
#
#   source deploy/dev-k3d/client-env.sh wendy      # writer
#   source deploy/dev-k3d/client-env.sh bob        # reader
#
# Then: claude mcp add --transport http --scope user team-memory https://memory.localtest.me/mcp
#       codex mcp add team_memory --url https://memory.localtest.me/mcp
#       codex mcp login team_memory
# and in Claude Code run /mcp to sign in. The Keycloak passwords are in the
# git-ignored deploy/dev-k3d/.local/env (<USER>_PASSWORD).
_tm_user=${1:?usage: source client-env.sh <user>}
_tm_dir=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)
_tm_home=$_tm_dir/.local/clients/$_tm_user
mkdir -p "$_tm_home/claude" "$_tm_home/codex"
export CLAUDE_CONFIG_DIR=$_tm_home/claude
export CODEX_HOME=$_tm_home/codex
export NODE_EXTRA_CA_CERTS=$_tm_dir/.local/ca.pem   # Claude Code (Node)
export SSL_CERT_FILE=$_tm_dir/.local/ca.pem         # Codex (Rust)
echo "client homes for $_tm_user: $_tm_home"
unset _tm_user _tm_dir _tm_home
