# agentdev webhook router (#3): the cluster's single webhook endpoint. Published as
# ghcr.io/valentinalexeev/agentdev-router (#28). No npm dependencies.
FROM node:24-bookworm-slim
WORKDIR /app
COPY router.js /app/router.js
USER 1000
EXPOSE 8080
CMD ["node", "/app/router.js"]
