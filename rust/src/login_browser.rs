//! Bundled Camoufox login browser. Every account owns a separate persistent
//! Firefox profile and a stable native fingerprint configuration.
use rand::RngCore;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::collections::HashMap;
use std::fs::{self, OpenOptions};
use std::io::{Cursor, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Mutex, OnceLock};
use std::time::Duration;

pub const VERSION: &str = "156.0.1-beta.36";
const PAYLOAD: &[u8] = include_bytes!("../../browser/camoufox-win-x64.zip");
static ENGINE_LOCK: Mutex<()> = Mutex::new(());
struct LoginChild {
    process: Child,
    url: String,
}
static CHILDREN: OnceLock<Mutex<HashMap<String, LoginChild>>> = OnceLock::new();

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Profile {
    pub id: String,
    pub realm: String,
    pub account_uid: Option<String>,
    audio_seed: u32,
    cpu_cores: u32,
}

impl Profile {
    pub fn prepare(root: &Path, realm: &str, account_uid: Option<&str>) -> Result<Self, String> {
        if !matches!(realm, "cn" | "global") {
            return Err("不支持的账号池".into());
        }
        let profiles = root.join("browser-profiles");
        fs::create_dir_all(&profiles).map_err(|e| e.to_string())?;
        if let Some(uid) = account_uid {
            for entry in fs::read_dir(&profiles).map_err(|e| e.to_string())? {
                let entry = entry.map_err(|e| e.to_string())?;
                let Ok(bytes) = fs::read(entry.path().join("identity.json")) else {
                    continue;
                };
                let Ok(profile) = serde_json::from_slice::<Self>(&bytes) else {
                    continue;
                };
                if profile.realm == realm && profile.account_uid.as_deref() == Some(uid) {
                    profile.validate()?;
                    if entry.file_name().to_str() != Some(profile.id.as_str()) {
                        return Err("账号登录环境目录与身份不一致".into());
                    }
                    return Ok(profile);
                }
            }
        }
        let mut random = [0u8; 16];
        rand::rngs::OsRng
            .try_fill_bytes(&mut random)
            .map_err(|e| e.to_string())?;
        let id: String = random.iter().map(|byte| format!("{byte:02x}")).collect();
        let profile = Self {
            id,
            realm: realm.into(),
            account_uid: account_uid.map(str::to_string),
            audio_seed: u32::from_le_bytes(random[..4].try_into().unwrap()).max(1),
            cpu_cores: [4, 8, 12, 16][random[4] as usize % 4],
        };
        let directory = profile.directory(root)?;
        fs::create_dir(&directory).map_err(|e| e.to_string())?;
        fs::create_dir(directory.join("profile")).map_err(|e| e.to_string())?;
        fs::write(
            directory.join("identity.json"),
            serde_json::to_vec_pretty(&profile).map_err(|e| e.to_string())?,
        )
        .map_err(|e| e.to_string())?;
        Ok(profile)
    }

    fn validate(&self) -> Result<(), String> {
        if self.id.len() != 32
            || !self.id.bytes().all(|c| c.is_ascii_hexdigit())
            || !matches!(self.realm.as_str(), "cn" | "global")
            || self.audio_seed == 0
        {
            return Err("账号登录环境身份无效".into());
        }
        Ok(())
    }

    fn directory(&self, root: &Path) -> Result<PathBuf, String> {
        self.validate()?;
        Ok(root.join("browser-profiles").join(&self.id))
    }

    pub fn check_account(&self, uid: &str) -> Result<(), String> {
        if self
            .account_uid
            .as_deref()
            .is_some_and(|expected| expected != uid)
        {
            return Err(
                "登录账号与所选环境不一致，已阻止覆盖凭证；请在该环境退出后登录正确账号".into(),
            );
        }
        Ok(())
    }

    pub fn bind(&self, root: &Path, uid: &str) -> Result<(), String> {
        self.check_account(uid)?;
        let mut bound = self.clone();
        bound.account_uid = Some(uid.into());
        // Credentials are saved separately; this file contains no tokens or cookies.
        let bytes = serde_json::to_vec_pretty(&bound).map_err(|e| e.to_string())?;
        fs::write(self.directory(root)?.join("identity.json"), bytes).map_err(|e| e.to_string())
    }

    fn config(&self) -> Value {
        json!({
            "audio:seed": self.audio_seed,
            "navigator.hardwareConcurrency": self.cpu_cores,
            "navigator.maxTouchPoints": 0
        })
    }

    fn command(&self, root: &Path, url: &str) -> Result<Command, String> {
        let engine = ensure_engine(root)?;
        let profile_dir = self.directory(root)?.join("profile");
        // Use native Windows fonts; no cross-OS font bundle is needed for login.
        fs::write(profile_dir.join("user.js"), "user_pref(\"gfx.bundled-fonts.activate\", 0);\nuser_pref(\"browser.shell.checkDefaultBrowser\", false);\n")
            .map_err(|e| e.to_string())?;
        let mut command = Command::new(&engine);
        command
            .args(["-no-remote", "-new-instance", "-profile"])
            .arg(&profile_dir)
            .arg("-url")
            .arg(url)
            .current_dir(engine.parent().unwrap())
            .env("CAMOU_CONFIG_1", self.config().to_string())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        // Never inherit another launcher's fingerprint chunks.
        for (key, _) in std::env::vars_os() {
            let name = key.to_string_lossy();
            if name == "CAMOU_CONFIG"
                || (name.starts_with("CAMOU_CONFIG_") && name != "CAMOU_CONFIG_1")
                || name.starts_with("CAMOU_PREFS")
            {
                command.env_remove(key);
            }
        }
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            command.creation_flags(super::sys::CREATE_NO_WINDOW);
        }
        Ok(command)
    }

    pub fn open(&self, root: &Path, url: &str) -> Result<(), String> {
        // Login navigation is restricted to the authorization service for this pool.
        let parsed = ureq::http::Uri::try_from(url).map_err(|_| "授权链接无效".to_string())?;
        let host = parsed.host().unwrap_or("");
        let allowed = match self.realm.as_str() {
            "global" => [
                "www.workbuddy.ai",
                "workbuddy.ai",
                "www.codebuddy.ai",
                "codebuddy.ai",
            ]
            .contains(&host),
            _ => ["www.codebuddy.cn", "codebuddy.cn", "copilot.tencent.com"].contains(&host),
        };
        if parsed.scheme_str() != Some("https") || !allowed {
            return Err("授权链接域名与所选账号池不一致".into());
        }
        let mut children = CHILDREN
            .get_or_init(|| Mutex::new(HashMap::new()))
            .lock()
            .map_err(|_| "浏览器状态不可用")?;
        if let Some(child) = children.get_mut(&self.id) {
            if child
                .process
                .try_wait()
                .map_err(|e| e.to_string())?
                .is_none()
            {
                return if child.url == url {
                    Ok(())
                } else {
                    Err("该账号环境仍有登录窗口，请关闭该窗口后再打开新的授权链接".into())
                };
            }
        }
        let mut child = self
            .command(root, url)?
            .spawn()
            .map_err(|e| format!("内置指纹浏览器启动失败: {e}"))?;
        std::thread::sleep(Duration::from_millis(600));
        if let Some(status) = child.try_wait().map_err(|e| e.to_string())? {
            return Err(format!(
                "内置指纹浏览器提前退出（{status}），请关闭该环境的旧窗口后重试"
            ));
        }
        children.insert(
            self.id.clone(),
            LoginChild {
                process: child,
                url: url.into(),
            },
        );
        Ok(())
    }
}

pub fn ensure_engine(root: &Path) -> Result<PathBuf, String> {
    let _guard = ENGINE_LOCK.lock().map_err(|_| "浏览器内核状态不可用")?;
    let destination = root.join("bin").join(format!("camoufox-{VERSION}"));
    let exe = destination.join("camoufox.exe");
    if exe.is_file() && destination.join(".complete").is_file() {
        return Ok(exe);
    }
    if destination.exists() {
        return Err("内置浏览器目录不完整，请关闭浏览器后将该目录移走再重试".into());
    }
    let mut archive = zip::ZipArchive::new(Cursor::new(PAYLOAD)).map_err(|e| e.to_string())?;
    let staging = root
        .join("bin")
        .join(format!("camoufox-{VERSION}.extract-{}", std::process::id()));
    fs::create_dir_all(staging.parent().unwrap()).map_err(|e| e.to_string())?;
    fs::create_dir(&staging).map_err(|e| e.to_string())?;
    for index in 0..archive.len() {
        let mut entry = archive.by_index(index).map_err(|e| e.to_string())?;
        let relative = entry.enclosed_name().ok_or("浏览器内核压缩包路径无效")?;
        let target = staging.join(relative);
        if entry.is_dir() {
            fs::create_dir_all(target).map_err(|e| e.to_string())?;
        } else {
            fs::create_dir_all(target.parent().unwrap()).map_err(|e| e.to_string())?;
            let mut file = OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(target)
                .map_err(|e| e.to_string())?;
            std::io::copy(&mut entry, &mut file).map_err(|e| e.to_string())?;
            file.flush().map_err(|e| e.to_string())?;
        }
    }
    if !staging.join("camoufox.exe").is_file() || !staging.join("xul.dll").is_file() {
        return Err("内置浏览器缺少必要文件".into());
    }
    fs::write(staging.join(".complete"), VERSION).map_err(|e| e.to_string())?;
    fs::rename(&staging, &destination).map_err(|e| e.to_string())?;
    Ok(exe)
}

#[cfg(test)]
mod tests;
