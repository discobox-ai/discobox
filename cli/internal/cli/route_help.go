package cli

import "github.com/discobox-ai/discobox/pkg/secretroute"

// routeHelp explains the route syntax, in the Long help of every command
// that takes --route. A host is exact: what lies beneath one is *.h, and help
// that says otherwise describes routes as they were before ADR 26-10-27-880.
const routeHelp = `A route is ` + secretroute.Syntax + `,
one per --route:

  METHODS   upper-case methods joined by commas (GET,HEAD); left out, any
  HOSTS     hosts joined by commas. A host is exact: *.example.com is every
            host beneath example.com but not example.com itself, and * alone
            is any host. A :port is dropped, so a host is every port of it
  PATHS     from the first /, several joined by ,/ (api.github.com/a/**,/b).
            A path is matched whole, a segment at a time and ignoring case:
            * is one segment, ** (last only) any number, so /repos/acme is
            that one path and /repos/acme/** it and everything beneath it.
            Left out, any path
  graphql=  the GraphQL operations allowed (query, mutation, subscription, or
            * for any), then optionally :FIELDS, the root fields they may
            select, each name allowed one trailing *
  mcp=      the MCP tools a tools/call may name, or * for any
  =blocked  refuses what the route is the most specific route for: a
            carve-out inside a wider route

  '*'
  api.github.com
  'GET api.github.com/repos/*/*/pulls/**'
  'api.github.com/graphql graphql=mutation:createPullRequest'
  'api.github.com/repos/*/*/pulls/*/merge=blocked'

A request must fall on a route of every list it is held to; within a list the
most specific route it falls on decides, host first, then path.`
