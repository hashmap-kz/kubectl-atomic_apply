## A brief example of usage

### Prerequisites:

You can use a Kind cluster for testing. If you haven’t installed it yet, check out
the [Kind installation guide](https://kind.sigs.k8s.io/).

### Deploy:

Execute scripts one by one:

```
# prepare kind cluster
bash 00-setup-kind.sh

# deploy
bash 01-deploy.sh
```

### Check Local Registry:

```
curl -X GET http://localhost:5000/v2/_catalog
curl -X GET http://localhost:5000/v2/pgrwl/tags/list
```

### Endpoints:

- grafana: http://localhost:30270

  ```
  User: admin
  Pass: admin
  ```

- minio: https://localhost:30267

  ```
  User: minioadmin
  Pass: minioadmin123
  ```
