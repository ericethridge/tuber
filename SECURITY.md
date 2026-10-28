# Security policy

## Supported versions

Security fixes are made to the latest released version. Please upgrade to the
latest chart and image before reporting an issue.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's private
vulnerability reporting instead: open the repository's **Security** tab and choose
**Report a vulnerability**. Include the affected version, steps to reproduce, and
the impact you see.

You can expect an acknowledgement within a few days. Fixes are released as a new
version, and the report is credited unless you prefer otherwise.

## Credentials

tuber needs a DoiT service account API token. Never put the token in Helm values,
`--set` flags, ConfigMaps, or Git. If a token is exposed, revoke it in the DoiT
Console and create a new one.
