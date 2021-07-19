KCP on kine
===========
Kine is an etcd shim developed as part of k3s. It allows you to select from
multiple storage backends while still presenting a subset of the etcd API
used by k8s.

Kine apparently doesn't support TLS for clients of its etcd API, although it
does support it between itself and some of the backend databases. I forked and
tweaked the project a little to enable TLS for the etcd API.

The kcp project makes a few etcd calls not supported by kine, but they are
informational, so I removed them. It also requires TLS, so I added an option to
drop it, which could make testing easier.

Installation and Setup
----------------------
To run kcp with kine as the etcd backend:

```bash
git clone git@github.com:csams/kine.git
git clone git@github.com:csams/kcp.git
cd kine && checkout add_tls
cd examples && ./generate-certs.sh
cd ../../kcp
git checkout enable_kine
```

Running
-------
Go back to the kine repo and run the following to expose kine as etcd with a sqlite backend:
```bash
go run main.go --listen-address=tcp://localhost:2379 --server-cert-file=examples/server.crt --server-key-file=examples/server.key
```

Go to the kcp repo and run the following to start kcp with kine as the etcd backend:
```bash
GODEBUG=x509ignoreCN=0 go run ./cmd/kcp start --etcd-servers=localhost:2379 --etcd-certfile=../kine/examples/server.crt --etcd-keyfile=../kine/examples/server.key --etcd-cafile=../kine/examples/ca.crt
```

Try it out
----------
Open another terminal, go to the kcp repo, and run the following to test:
```bash
KUBECONFIG=.kcp/data/admin.kubeconfig kubectl api-resources
```

Next browse to the kine repo:
```bash
sqlite3 db/state.db
sqlite> .tables
kine
sqlite> select * from kine;
<rows..>
```

Disable TLS
-----------
To disable TLS, run kine like
```bash
go run main.go --listen-address=tcp://localhost:2379
```

and run kcp like
```bash
go run ./cmd/kcp start --etcd-servers=localhost:2379 --use_tls=false
```

Postgres Backend
----------------
```bash
# run postgres with podman
podman run --name kine-postgres -p5432:5432 -e POSTGRES_USER=admin -e POSTGRES_PASSWORD=admin -d postgres

# start kine pointed at postgres
go run main.go --endpoint="postgres://admin:admin@localhost:5432/postgres?sslmode=disable" --listen-address=tcp://localhost:2379 --server-cert-file=examples/server.crt --server-key-file=examples/server.key

# start kcp as before
GODEBUG=x509ignoreCN=0 go run ./cmd/kcp start --etcd-servers=localhost:2379 --etcd-certfile=../kine/examples/server.crt --etcd-keyfile=../kine/examples/server.key --etcd-cafile=../kine/examples/ca.crt
```

You can log into postgres and view the kine tables in the postgres database.
```bash
psql -h localhost -U admin -d postgres -W
```
