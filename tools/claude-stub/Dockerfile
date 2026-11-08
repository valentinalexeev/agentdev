# Fake Anthropic API for the compatibility e2e (#178). Test only: never deploy it in an instance.
FROM node:24-alpine
COPY stub.js /stub.js
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["node", "/stub.js"]
