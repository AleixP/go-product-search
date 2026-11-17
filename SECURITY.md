# Security policy

go-product-search is a demonstration project meant for local use. The API has no
authentication, and the compose file turns off the OpenSearch security plugin
and TLS. Do not expose either service to a network. See "Local use only" in the
README.

## Supported versions

Only the latest commit on `main`.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private vulnerability reporting:
the "Report a vulnerability" button under the Security tab of this repository.

I will answer as soon as I can. This is a personal project, so I can't promise a
response time or a fix date.

## Scope

In scope: the Go code in this repository, for example input validation in the
API or the ingest command, or a vulnerable dependency.

Out of scope: running the service exposed to a network against the README's
instructions, and vulnerabilities in OpenSearch or Docker themselves.
