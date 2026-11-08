package hubapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const constraintsFirstPage = "GET /api/v1/admin/access-constraints?nameContains=lever-remote-ceiling%3Aa%40b&pageSize=200"

// The exact name can be past the first page of substring matches: "none
// found" would make the caller create a duplicate.
func TestAccessConstraintsNamedReadsEveryPage(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		constraintsFirstPage: {status: 200, body: `{"items":[
			{"id":"ac-9","name":"lever-remote-ceiling:xa@b","subject":{"kind":"principal"},"scope":{"type":"system"},"status":"active","revision":"1"}
		],"nextPageToken":"200","totalCount":2}`},
		constraintsFirstPage + "&pageToken=200": {status: 200, body: `{"items":[
			{"id":"ac-1","name":"lever-remote-ceiling:a@b","subject":{"kind":"principal"},"scope":{"type":"system"},"status":"active","revision":"3"}
		],"totalCount":2}`},
	}}
	got, err := (&Client{T: f}).AccessConstraintsNamed(context.Background(), "lever-remote-ceiling:a@b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ac-1" {
		t.Fatalf("got %+v, want ac-1 from page 2 (calls %v)", got, f.calls)
	}
}

// A hub that never stops handing out pages, or repeats one, is an error, not
// "none found".
func TestAccessConstraintsNamedStopsOnEndlessPages(t *testing.T) {
	endless := &fakeDoer{replies: map[string]reply{}}
	endless.replies[constraintsFirstPage] = reply{status: 200, body: `{"items":[],"nextPageToken":"t1"}`}
	for i := 1; i <= listPageLimit; i++ {
		endless.replies[fmt.Sprintf("%s&pageToken=t%d", constraintsFirstPage, i)] =
			reply{status: 200, body: fmt.Sprintf(`{"items":[],"nextPageToken":"t%d"}`, i+1)}
	}
	if _, err := (&Client{T: endless}).AccessConstraintsNamed(context.Background(), "lever-remote-ceiling:a@b"); err == nil ||
		!strings.Contains(err.Error(), "did not end") {
		t.Fatalf("endless pages: err = %v", err)
	}
	loop := &fakeDoer{replies: map[string]reply{
		constraintsFirstPage:                  {status: 200, body: `{"items":[],"nextPageToken":"a"}`},
		constraintsFirstPage + "&pageToken=a": {status: 200, body: `{"items":[],"nextPageToken":"a"}`},
	}}
	if _, err := (&Client{T: loop}).AccessConstraintsNamed(context.Background(), "lever-remote-ceiling:a@b"); err == nil ||
		!strings.Contains(err.Error(), "repeated") {
		t.Fatalf("repeated token: err = %v", err)
	}
}

// A project past the first page is found, and a same-named project on a
// later page still makes the name ambiguous.
func TestProjectIDReadsEveryPage(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects": {status: 200, body: `{"projects":[
			{"id":"11111111-1111-1111-1111-111111111111","name":"other","slug":"other"}
		],"nextCursor":"c/2+","totalCount":2}`},
		"GET /api/v1/projects?cursor=c%2F2%2B": {status: 200, body: `{"projects":[
			{"id":"22222222-2222-2222-2222-222222222222","name":"lever","slug":"lever-1"}
		],"totalCount":2}`},
	}}
	id, err := (&Client{T: f}).ProjectID(context.Background(), "lever", "hub")
	if err != nil || id != leverUUID {
		t.Fatalf("ProjectID = %q, %v; want %s from page 2 (calls %v)", id, err, leverUUID, f.calls)
	}

	dup := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects": {status: 200, body: `{"projects":[
			{"id":"22222222-2222-2222-2222-222222222222","name":"lever","slug":"lever-1"}
		],"nextCursor":"p2"}`},
		"GET /api/v1/projects?cursor=p2": {status: 200, body: `{"projects":[
			{"id":"33333333-3333-3333-3333-333333333333","name":"lever","slug":"lever-2"}
		]}`},
	}}
	if _, err := (&Client{T: dup}).ProjectID(context.Background(), "lever", "hub"); err == nil ||
		!strings.Contains(err.Error(), "2 projects named") {
		t.Fatalf("a duplicate on page 2 must be ambiguous, got %v", err)
	}

	none := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects":           {status: 200, body: `{"projects":[{"id":"1","name":"a","slug":"a"}],"nextCursor":"p2"}`},
		"GET /api/v1/projects?cursor=p2": {status: 200, body: `{"projects":[{"id":"2","name":"b","slug":"b"}]}`},
	}}
	_, err = (&Client{T: none}).ProjectID(context.Background(), "lever", "hub")
	if err == nil || !isNoProject(err) || !strings.Contains(err.Error(), "listed 2 project(s)") {
		t.Fatalf("no match across pages: %v", err)
	}

	loop := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects":          {status: 200, body: `{"projects":[],"nextCursor":"x"}`},
		"GET /api/v1/projects?cursor=x": {status: 200, body: `{"projects":[],"nextCursor":"x"}`},
	}}
	if _, err := (&Client{T: loop}).ProjectID(context.Background(), "lever", "hub"); err == nil ||
		!strings.Contains(err.Error(), "repeated") || isNoProject(err) {
		t.Fatalf("a repeated cursor must be an error, not no-project: %v", err)
	}
}

func isNoProject(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.NoProject
}
