package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// contact_files and share_file (remote.files): the facts about a file come
// from the broker's host record, never from what the agent finds in its
// .lever-files directory.

const contactFilesDescription = "List one login's files with you, from lever's host record: the files that login uploaded to you " +
	"and the files you shared with it (id, login, name, size, sha256, path, at), and that login's in_dir and out_dir. " +
	"contact is required: the login whose verified message you are answering. Use a file only in that login's conversation, " +
	"and only when it is listed here with the same sha256. Refusals (note): contact-required, not-a-contact, rate."

const shareFileDescription = "Share one file with a login: write it into that login's out_dir (from contact_files) first, " +
	"then call this with the login and the file's path. Lever records its sha256; the login downloads exactly those bytes. " +
	"Refusals: not-a-contact, bad-path, not-found, symlink, hard-link, not-a-file, too-large, extension, rate, off, unavailable."

// fileToolSchemas are the tools/list entries of contact_files and
// share_file: plain top-level objects (no combinator, #24).
func fileToolSchemas(strProp func(string) map[string]any) []any {
	return []any{
		map[string]any{"name": "contact_files", "description": contactFilesDescription,
			"inputSchema": map[string]any{"type": "object", "required": []string{"contact"}, "properties": map[string]any{
				"contact": strProp("the login whose verified message you are answering, exactly as message_verify named it"),
			}}},
		map[string]any{"name": "share_file", "description": shareFileDescription,
			"inputSchema": map[string]any{"type": "object", "required": []string{"to", "path"},
				"properties": map[string]any{
					"to":   strProp("a login from contact_files, exactly as listed"),
					"path": strProp("the file's path, directly inside that login's out_dir"),
				}}},
	}
}

// contactFilesTool returns the broker's answer to PathFilesList unchanged.
func contactFilesTool(s *MCPServer, ctx context.Context, args map[string]string) (string, error) {
	contact := strings.TrimSpace(args["contact"])
	if contact == "" {
		return "", invalidParams{errors.New(`"contact" is required: the login whose verified message you are answering`)}
	}
	var raw json.RawMessage
	err := httpjson.Post(ctx, s.client, s.brokerURL+wire.PathFilesList, wire.FilesListRequest{Contact: contact}, &raw)
	return string(raw), err
}

// shareFileTool passes to unchanged (the broker matches it, case-folded,
// against the configured logins; it never guesses past spaces) and the path
// trimmed. A refusal is a normal result with its reason word.
func shareFileTool(s *MCPServer, ctx context.Context, args map[string]string) (string, error) {
	to, p := args["to"], strings.TrimSpace(args["path"])
	if to == "" || p == "" {
		return "", invalidParams{errors.New(`"to" and "path" are required`)}
	}
	var raw json.RawMessage
	err := httpjson.Post(ctx, s.client, s.brokerURL+wire.PathFilesShare, wire.FileShareRequest{To: to, Path: p}, &raw)
	return string(raw), err
}
