package retrieve

import (
	"context"

	"github.com/arctop/seamless/internal/access"
)

// ProjectPromptRecall is PromptRecall for a project the caller already
// resolved -- a conversation's home, with isolation.requests on (the hooks'
// conversationScope) -- reading also beside it: the targets of the
// conversation's live grants, each of which the caller judged readable with
// store.CanRead over the conversation's clearance, as recall's Also is. It
// reads no cwd. A project the caller may not read (access.Visible: a home
// locked to it, under peer_auth) recalls nothing, as a cwd in a locked
// project does, and a hit outside the caller's reach is dropped as in
// PromptRecall.
func (s *Service) ProjectPromptRecall(ctx context.Context, project string, also []string, prompt string) (string, []string, error) {
	tokens := promptTokenize(prompt)
	if len(tokens) == 0 {
		return "", nil, nil
	}
	if !access.Visible(ctx, project) {
		return "", nil, nil
	}
	return s.promptRecallIn(ctx, newPromptScope(project, also), tokens)
}
