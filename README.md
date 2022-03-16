# gcp quota comparer

This tool is used to compare similarly named projects in (Google Cloud Platform)[https://console.cloud.google.com] based upon a project filter and regex strings to determine compatibility. This then will loop through all projects and regions and output any differences between quotas.

## Usage

### Manually

```sh
go install
go build
./gcp-quota-comparer --from="labels.environment=dev" --to="labels.environment=staging"
```

### VS Code

`cp ./env.example .env`
modify the environmental values using the properties specified in `main.go`
Use `Launch Package`