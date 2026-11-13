# Security policy

## Supported versions

The latest released version receives security fixes.

## Reporting a vulnerability

Please report security vulnerabilities privately by email to vaclav@ambroz.email.

Include:
- A description of the vulnerability and its impact
- Steps to reproduce or a proof-of-concept
- Any suggested mitigations

You will receive a response within 72 hours. We will coordinate a fix and
disclosure timeline with you before publishing.

Do not open a public GitHub issue for security vulnerabilities.

## Release integrity

All releases are signed with Minisign. The signing public key is published at
the project homepage alongside the release manifests. Verify releases with:

```sh
minisign -Vm manifest.json -p run-ai-release.minisign.pub
```
