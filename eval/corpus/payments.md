# Payment retries

The payment gateway can return transient failures. We retry with exponential
backoff: 200ms, then 400ms, then 800ms, capped at three attempts. The backoff is
configured in payments.yaml under retry.backoff. A permanent decline is never
retried and is surfaced to the customer immediately.
