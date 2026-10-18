# Security Policy

## Supported Versions

| Version | Supported |
| ------- | --------- |
| 0.x     | Yes       |
| < 0.x   | No        |

playscape is in **beta** (pre-1.0). Only the latest 0.x release receives security fixes.

## Reporting a Vulnerability

Please report vulnerabilities privately via [GitHub Security Advisories](https://github.com/urmzd/playscape/security/advisories/new). Do not open a public issue for security reports.

You can expect an acknowledgment within 72 hours. Once a fix is available, we will coordinate disclosure with you.

The sandbox layer (`pkg/sandbox`) is a security boundary: any way for a tool to read or write workspace keys outside its policy's areas is a vulnerability, not a bug.
