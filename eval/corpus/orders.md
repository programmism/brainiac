# OrderService

OrderService is the checkout backend. It persists every order to Postgres so that
a crash never loses an in-flight purchase — durability is the reason we write to a
relational store rather than an in-memory cache. After persisting, OrderService
publishes an OrderPlaced event to Kafka for downstream fulfillment and analytics.
