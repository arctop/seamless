package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/capture"
	"github.com/arctop/seamless/internal/core"
)

// captureBodyMaxRunes caps a captured page's stored body so one huge page cannot
// bloat a note file (and its embedding).
const captureBodyMaxRunes = 50000

func captureURLTool() mcp.Tool {
	return mcp.NewTool("capture_url", hintFetch(),
		mcp.WithDescription("Fetch a web page (SSRF-guarded: private/loopback addresses are rejected) and save its readable content as a note. Returns the new note's id."),
		mcp.WithString("url", mcp.Required(), mcp.Description("http(s) URL to capture")),
		mcp.WithString("project", mcp.Description(writeProjectArgDesc)),
	)
}

func (s *Server) handleCaptureURL(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawURL := argString(req, "url")
	if rawURL == "" {
		return errResult("capture_url", errors.New("url is required"))
	}
	// Resolve the destination scope before fetching: a captured page is a durable
	// note, so an ambiguous scope must fail fast rather than do an SSRF fetch for a
	// write we are only going to reject.
	project, err := s.resolveWriteScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("capture_url", err)
	}
	content, err := s.fetcher.FetchURL(ctx, rawURL)
	if err != nil {
		return errResult("capture_url", err)
	}
	note, err := s.capturedNote(ctx, content, project)
	if err != nil {
		return errResult("capture_url", err)
	}
	// A captured page is outside text by definition, landing straight in the
	// corpus where recall can serve it to every later session: it is screened
	// like any other note write.
	screen := s.screeningOn(ctx)
	var screenSession string
	var jd judged
	if screen {
		session := s.boundSession(ctx)
		screenSession = session
		if herr := s.judgeNoteWrite(ctx, "capture_url", note, core.Note{}, "", session, &jd); herr != nil {
			held, ok := asHeld(herr)
			if !ok {
				return errResult("capture_url", herr)
			}
			s.recordScreened(ctx, "capture_url", core.ItemKindNote, note.ID, project, session, "", jd)
			return heldResult(held.hold, map[string]any{"id": note.ID, "slug": note.Slug, "title": note.Title,
				"project": project, "source_url": content.URL})
		}
	}
	written, err := s.cfg.Files.WriteNote(ctx, note)
	if err != nil {
		return errResult("capture_url", err)
	}
	s.record(ctx, core.EventNoteWritten, s.boundSession(ctx), project, written.ID,
		map[string]any{"title": content.Title, "source_url": content.URL})
	if screen {
		s.recordScreened(ctx, "capture_url", core.ItemKindNote, written.ID, project, screenSession, "", jd)
	}
	return jsonResult(map[string]any{
		"id": written.ID, "slug": written.Slug, "title": written.Title,
		"project": project, "source_url": content.URL,
	})
}

// capturedNote builds the note capture_url writes for a fetched page: the
// page's text under a source line, capped, tagged as a capture and attributed
// to the writer -- the bound session's model and, for a restricted account,
// its account label.
func (s *Server) capturedNote(ctx context.Context, content *capture.URLContent, project string) (core.Note, error) {
	body := fmt.Sprintf("Source: %s\n\n---\n\n%s", content.URL, content.Body)
	if r := []rune(body); len(r) > captureBodyMaxRunes {
		body = string(r[:captureBodyMaxRunes]) + "\n\n[content truncated]"
	}
	id, err := core.NewID()
	if err != nil {
		return core.Note{}, err
	}
	now := time.Now().UTC()
	return core.Note{
		ID: id, Title: content.Title, Slug: core.Slugify(content.Title), Description: "Captured from " + content.URL,
		Project: project, Body: body, Tags: []string{"created-by:agent", "captured-url"},
		SourceURL: content.URL, Model: s.boundSessionModel(ctx), Account: access.AccountOf(ctx),
		Created: now, Updated: now,
	}, nil
}
