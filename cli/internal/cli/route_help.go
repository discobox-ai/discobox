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
  body=     places in a JSON request body, JSON pointers joined by ,/
            (body=/api_key). The credential goes there, as the whole string
            at one of them, and nowhere else: not in the headers, and not
            elsewhere in the body. A route without body= allows it in the
            headers and query, and at no body place
  ws=       places in the JSON messages a client sends on the WebSocket
            connection the route's request upgrades: a JSON array of
            {"at": POINTER, "when": {MEMBER: VALUE}}, quoted as one word.
            The credential goes there, as the whole string at the pointer,
            in a message whose top-level members hold every condition, and
            nowhere else. A route without ws=, * included, allows it in no
            message. The whole array is one JSON value; =blocked follows it
  =blocked  refuses what the route is the most specific route for: a
            carve-out inside a wider route

  '*'
  api.github.com
  'GET api.github.com/repos/*/*/pulls/**'
  'api.github.com/graphql graphql=mutation:createPullRequest'
  'api.github.com/repos/*/*/pulls/*/merge=blocked'
  'POST app.boxd.sh/api/v1/auth/token body=/api_key'
  'GET gateway.discord.gg,*.discord.gg ws=[{"at":"/d/token","when":{"op":2}},{"at":"/d/token","when":{"op":6}}]'

A request must fall on a route of every list it is held to; within a list the
most specific route it falls on decides, host first, then path.`
