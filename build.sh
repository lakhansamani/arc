#!/bin/bash
git clone https://$1@github.com/appbaseio-confidential/arc-noss
cd arc-noss
currentBilling=""
if [[ "$2" == "true" ]]; then
    currentBilling="self_hosted"
fi
if [[ "$3" == "true" ]]; then
    currentBilling="cluster"
fi
if [[ "$4" == "true" ]]; then
    currentBilling="byoc"
fi

make clean && GO111MODULE=on BILLING=$2 CLUSTER_BILLING=$3 HOSTED_BILLING=$4 PLAN_REFRESH_INTERVAL=$5 VERSION=$6 make
mkdir -p build/appbaseio
sudo mv $GOPATH/pkg/mod/github.com/appbaseio/* build/appbaseio/
chmod -R 777 build
zip -r "arc-linux_$currentBilling.zip" build go

# upload to github
# Define variables.
owner="lakhansamani"
repo="arc"
tag="$6"
github_api_token="$1"
filename="arc-linux_$currentBilling.zip"
GH_API="https://api.github.com"
GH_REPO="$GH_API/repos/$owner/$repo"
GH_TAGS="$GH_REPO/releases/latest"
AUTH="Authorization: token $github_api_token"
WGET_ARGS="--content-disposition --auth-no-challenge --no-cookie"
CURL_ARGS="-LJO#"

# Validate token.
curl -o /dev/null -sH "$AUTH" $GH_REPO || { echo "Error: Invalid repo, token or network issue!";  exit 1; }

# Read asset tags.
response=$(curl -sH "$AUTH" $GH_TAGS)

# Get ID of the asset based on given filename.
eval $(echo "$response" | grep -m 1 "id.:" | grep -w id | tr : = | tr -cd '[[:alnum:]]=')
[ "$id" ] || { echo "Error: Failed to get release id for tag: $tag"; echo "$response" | awk 'length($0)<100' >&2; exit 1; }

# Upload asset
echo "Uploading asset... "

# Construct url
GH_ASSET="https://uploads.github.com/repos/$owner/$repo/releases/$id/assets?name=$(basename $filename)"

curl "$GITHUB_OAUTH_BASIC" --data-binary @"$filename" -H "Authorization: token $github_api_token" -H "Content-Type: application/octet-stream" $GH_ASSET
