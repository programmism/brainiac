# Authentication

Services authenticate to each other with short-lived bearer tokens issued by the
identity service. Tokens expire after fifteen minutes and are rotated automatically.
We deliberately avoid long-lived API keys because a leaked key is hard to revoke.
