#!/usr/bin/env bash
set -euxo pipefail
cd "$(dirname $(dirname "$0"))"

tfget() {
  terraform -chdir=tf show -json |
    jq -r ".values.root_module.resources[] | select(.address == \"$1\") | .values.$2"
}

repourl=$(tfget aws_ecr_repository.repo repository_url)
skopeo --insecure-policy list-tags docker://$repourl >&/dev/null ||
  aws ecr get-login-password | skopeo login --username AWS --password-stdin ${repourl%%/*}

script=$(nix-build --no-out-link -A image)
tag=$(basename $script | cut -c-12)
$script | gzip --fast | skopeo --insecure-policy copy \
  docker-archive:/dev/stdin \
  docker://$repourl:$tag

export TF_VAR_differ_image_tag=$tag
terraform -chdir=tf apply

echo ============
echo "URL is: $(tfget aws_lambda_function_url.differ function_url)"
