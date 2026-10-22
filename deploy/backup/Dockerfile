# PostgreSQL 17 + pgvector with pgBackRest, for the restore drill and as a
# reference for adding pgBackRest to a self-managed PostgreSQL host.
# pgvector/pgvector:0.8.1-pg17 (Debian, PGDG apt repository preconfigured)
FROM pgvector/pgvector:0.8.1-pg17@sha256:3e8b3adfd27b5707128f60956f62a793c3c9326ea8cfaf0eab7adccb5d700b21
RUN apt-get update \
 && apt-get install -y --no-install-recommends pgbackrest \
 && rm -rf /var/lib/apt/lists/* \
 && install -d -o postgres -g postgres -m 750 /var/lib/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest
COPY pgbackrest.conf /etc/pgbackrest/pgbackrest.conf
