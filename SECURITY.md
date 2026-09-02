# Security policy

## Reporting a vulnerability

Please report security vulnerabilities privately through
[GitHub Security Advisories](https://github.com/ih8d8/yt-dlp-manager/security/advisories/new).
Do not open a public issue for a vulnerability before a fix is available.

Include the affected version, deployment method, reproduction steps, impact,
and any suggested mitigation you have. Reports should avoid including real
credentials, session cookies, setup tokens, downloaded media, or other private
data.

You should receive an acknowledgement within seven days. If the report is
confirmed, fixes and disclosure will be coordinated through the advisory.

## Supported versions

Until the project publishes its first stable release, only the latest release
is supported. After stable releases begin, this section will list the supported
release lines explicitly.

## Deployment responsibility

The default Compose deployment binds to loopback. Remote deployments should
terminate TLS at a reverse proxy, set `YTDLP_MANAGER_SECURE_COOKIE=true`, and
keep authentication enabled. Plain-HTTP LAN exposure is not considered a
secure production configuration.

Behind a proxy, `YTDLP_MANAGER_TRUSTED_PROXIES` may optionally name that
proxy's address. Login rate limiting otherwise sees only the proxy and puts
every client in one backoff bucket, so one person's failed guesses briefly
delay everyone's sign-in. The backoff is capped at a minute, so this is a
nuisance rather than a lockout and the setting is not required. Set it only for
proxies you run, and only if yours overwrites `X-Forwarded-For` rather than
appending to a client-supplied value.
