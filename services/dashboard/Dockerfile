# syntax=docker/dockerfile:1.7
FROM node:24.13.0-alpine3.23@sha256:cd6fb7efa6490f039f3471a189214d5f548c11df1ff9e5b181aa49e22c14383e AS dependencies
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

FROM dependencies AS build
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1 \
    BETTER_AUTH_SECRET=build-only-secret-00000000000000000000000000000000 \
    BETTER_AUTH_URL=http://localhost:3000 \
    BETTER_AUTH_DATABASE_PATH=:memory:
RUN npm run build

FROM dependencies AS admin
COPY . .
ENV NODE_ENV=production
RUN addgroup -S -g 10001 dashboard && adduser -S -D -H -u 10001 -G dashboard dashboard
USER 10001:10001

FROM node:24.13.0-alpine3.23@sha256:cd6fb7efa6490f039f3471a189214d5f548c11df1ff9e5b181aa49e22c14383e AS runtime
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 PORT=3000 HOSTNAME=0.0.0.0
WORKDIR /app
RUN addgroup -S -g 10001 dashboard && adduser -S -D -H -u 10001 -G dashboard dashboard
COPY --from=build --chown=10001:10001 /app/.next/standalone ./
COPY --from=build --chown=10001:10001 /app/.next/static ./.next/static
USER 10001:10001
EXPOSE 3000
CMD ["node", "server.js"]
