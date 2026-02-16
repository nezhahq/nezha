import os
import time
import requests
from github import Github

ATOMGIT_API = "https://api.atomgit.com/api/v5"
ATOMGIT_OWNER = "naiba"
ATOMGIT_REPO = "nezha-dashboard"
GITHUB_REPO = "nezhahq/nezha"


def get_github_latest_release():
    g = Github()
    repo = g.get_repo(GITHUB_REPO)
    release = repo.get_latest_release()
    if not release:
        print("No releases found.")
        return

    print(f"Latest release tag is: {release.tag_name}")
    print(f"Latest release info is: {release.body}")
    files = []
    for asset in release.get_assets():
        url = asset.browser_download_url
        name = asset.name

        response = requests.get(url)
        if response.status_code == 200:
            with open(name, "wb") as f:
                f.write(response.content)
            print(f"Downloaded {name}")
        else:
            print(f"Failed to download {name}")
        files.append(get_abs_path(name))
    sync_to_atomgit(release.tag_name, release.body, files)


def sync_to_atomgit(tag, body, files):
    access_token = os.environ["ATOMGIT_PAT"]
    release_api_uri = f"{ATOMGIT_API}/repos/{ATOMGIT_OWNER}/{ATOMGIT_REPO}/releases"

    auth_headers = {"Authorization": f"Bearer {access_token}"}
    release_data = {
        "tag_name": tag,
        "name": tag,
        "body": body,
        "prerelease": False,
        "target_commitish": "master",
    }

    release_resp = None
    for attempt in range(3):
        try:
            release_resp = requests.post(
                release_api_uri, json=release_data, headers=auth_headers, timeout=30
            )
            release_resp.raise_for_status()
            break
        except requests.exceptions.Timeout:
            print(
                f"Create release timed out, retrying in 30s... (attempt {attempt + 1})"
            )
            time.sleep(30)
        except requests.exceptions.RequestException as err:
            print(f"Create release failed: {err}")
            if release_resp is not None:
                print(f"Response: {release_resp.text}")
            break

    if release_resp is None or release_resp.status_code not in (200, 201):
        print("Failed to create release on AtomGit, aborting.")
        return

    print(f"Created release {tag} on AtomGit")

    for file_path in files:
        upload_asset(access_token, tag, file_path)

    print("Sync is completed!")


def upload_asset(access_token, tag, file_path):
    file_name = os.path.basename(file_path)
    upload_url_api = (
        f"{ATOMGIT_API}/repos/{ATOMGIT_OWNER}/{ATOMGIT_REPO}"
        f"/releases/{tag}/upload_url?file_name={file_name}"
    )

    for attempt in range(3):
        try:
            resp = requests.get(
                upload_url_api,
                headers={"Authorization": f"Bearer {access_token}"},
                timeout=30,
            )
            resp.raise_for_status()
            upload_info = resp.json()

            obs_url = upload_info["url"]
            obs_headers = upload_info["headers"]

            with open(file_path, "rb") as f:
                put_resp = requests.put(
                    obs_url, headers=obs_headers, data=f, timeout=120
                )

            if put_resp.text.strip() == "success" or put_resp.status_code in (200, 201):
                print(f"Uploaded {file_name}")
                return
            else:
                print(
                    f"Upload {file_name} failed: {put_resp.status_code} {put_resp.text}"
                )
        except requests.exceptions.RequestException as err:
            print(f"Upload {file_name} attempt {attempt + 1} failed: {err}")
            time.sleep(10)

    print(f"Failed to upload {file_name} after 3 attempts")


def get_abs_path(path):
    return os.path.join(os.getcwd(), path)


get_github_latest_release()
