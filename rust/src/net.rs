// WorkBuddy2API · 网络层
// OAuth 设备授权 / 每日签到 / /v1/models 拉取
// 移植自 addaccount.py 与 scripts/list_models.py

use serde_json::{json, Value};
use std::fs;

use crate::sys;

pub const UA: &str = "CLI/2.63.2 CodeBuddy/2.63.2";

pub struct Realm {
    pub base: &'static str,
    pub origin: &'static str,
}

impl Realm {
    pub fn of(key: &str) -> Realm {
        match key {
            "global" => Realm {
                base: "https://www.workbuddy.ai",
                origin: "https://www.workbuddy.ai",
            },
            _ => Realm {
                base: "https://copilot.tencent.com",
                origin: "https://www.codebuddy.cn",
            },
        }
    }
}

pub fn base_headers(origin: &str, token: Option<&str>) -> Vec<(String, String)> {
    let mut h = vec![
        ("Content-Type".to_string(), "application/json".to_string()),
        (
            "Accept".to_string(),
            "application/json, text/plain, */*".to_string(),
        ),
        ("X-Requested-With".to_string(), "XMLHttpRequest".to_string()),
        ("Origin".to_string(), origin.to_string()),
        ("Referer".to_string(), format!("{}/", origin)),
        ("User-Agent".to_string(), UA.to_string()),
    ];
    if let Some(t) = token {
        h.push(("Authorization".to_string(), format!("Bearer {}", t)));
    }
    h
}

/// 统一 JSON 调用：解 {code,msg,data}，code!=0 视为失败。
pub fn http_json(
    url: &str,
    method: &str,
    body: Option<&Value>,
    headers: &[(String, String)],
) -> Result<Value, String> {
    let agent = ureq::agent();
    let text = if method == "POST" {
        let mut r = agent.post(url);
        for (k, v) in headers {
            r = r.header(k.as_str(), v.as_str());
        }
        let payload = match body {
            Some(b) => serde_json::to_string(b).map_err(|e| e.to_string())?,
            None => "{}".to_string(),
        };
        let mut res = r
            .send(payload.as_str())
            .map_err(|e| format!("{}", e))?;
        res.body_mut().read_to_string().map_err(|e| e.to_string())?
    } else {
        let mut r = agent.get(url);
        for (k, v) in headers {
            r = r.header(k.as_str(), v.as_str());
        }
        let mut res = r.call().map_err(|e| format!("{}", e))?;
        res.body_mut().read_to_string().map_err(|e| e.to_string())?
    };

    let env: Value = serde_json::from_str(&text)
        .map_err(|_| format!("响应不是合法 JSON: {}", &text.chars().take(200).collect::<String>()))?;
    let code = env.get("code").and_then(|v| v.as_i64()).unwrap_or(0);
    if code != 0 {
        return Err(format!(
            "code={} msg={}",
            code,
            env.get("msg").and_then(|v| v.as_str()).unwrap_or("")
        ));
    }
    Ok(env.get("data").cloned().unwrap_or_else(|| json!({})))
}

/// 第一步：申请设备授权码，返回 (state, authUrl)
pub fn auth_state(realm_key: &str) -> Result<(String, String), String> {
    let r = Realm::of(realm_key);
    let url = format!("{}/v2/plugin/auth/state?platform=CLI", r.base);
    let h = base_headers(r.origin, None);
    let d = http_json(&url, "POST", Some(&json!({})), &h)?;
    let state = d
        .get("state")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let auth_url = d
        .get("authUrl")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    if state.is_empty() || auth_url.is_empty() {
        return Err("返回缺少 state / authUrl".into());
    }
    Ok((state, auth_url))
}

/// 国内版每日签到（幂等，失败不影响登录流程）
pub fn daily_checkin(token: &str, uid: &str, ent: &str, domain: &str) -> String {
    let agent = ureq::agent();
    let mut r = agent.post("https://www.codebuddy.cn/v2/billing/meter/daily-checkin");
    r = r
        .header("Authorization", &format!("Bearer {}", token))
        .header("Accept", "application/json")
        .header("Content-Type", "application/json")
        .header("X-User-Id", uid);
    if !ent.is_empty() {
        r = r.header("X-Enterprise-Id", ent).header("X-Tenant-Id", ent);
    }
    if !domain.is_empty() {
        r = r.header("X-Domain", domain);
    }
    match r.send("{}") {
        Ok(mut res) => {
            let t = res.body_mut().read_to_string().unwrap_or_default();
            let v: Value = serde_json::from_str(&t).unwrap_or_else(|_| json!({}));
            v.get("msg")
                .and_then(|x| x.as_str())
                .unwrap_or("已完成")
                .to_string()
        }
        Err(e) => format!("签到失败: {}", e),
    }
}

pub struct SavedAccount {
    pub file: String,
    pub uid: String,
    pub nick: String,
    pub checkin: String,
}

/// 第二步：用 state 换 token → 取账号信息 → 落盘 auths/ → 国内版签到
pub fn finish_login(realm_key: &str, state: &str, profile: &crate::login_browser::Profile) -> Result<SavedAccount, String> {
    if profile.realm != realm_key {
        return Err("登录环境与账号池不一致".into());
    }
    let r = Realm::of(realm_key);

    let tok = http_json(
        &format!("{}/v2/plugin/auth/token?state={}", r.base, state),
        "GET",
        None,
        &base_headers(r.origin, None),
    )?;
    let access = tok
        .get("accessToken")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    if access.is_empty() {
        return Err("未拿到 accessToken（请先在浏览器完成登录）".into());
    }

    let acct = http_json(
        &format!("{}/v2/plugin/login/account?state={}", r.base, state),
        "GET",
        None,
        &base_headers(r.origin, Some(&access)),
    )
    .unwrap_or_else(|_| json!({}));

    let uid = acct
        .get("uid")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    if uid.is_empty() {
        return Err("无法获取 uid".into());
    }
    profile.check_account(&uid)?;

    let nick = acct
        .get("nickname")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let ent = acct
        .get("enterpriseId")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let domain = tok
        .get("domain")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let expires_in = tok.get("expiresIn").and_then(|v| v.as_i64()).unwrap_or(0);

    let payload = json!({
        "account": {"uid": uid, "enterpriseId": ent, "nickname": nick},
        "auth": {
            "accessToken": access,
            "refreshToken": tok.get("refreshToken").and_then(|v| v.as_str()).unwrap_or(""),
            "expiresAt": sys::now_secs() + expires_in,
            "domain": domain,
            "realm": realm_key
        }
    });

    let dir = sys::auths_dir();
    fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    let file = format!("workbuddy-{}.json", uid);
    let body = serde_json::to_string_pretty(&payload).map_err(|e| e.to_string())?;
    profile.bind(&sys::root(), &uid)?;
    fs::write(dir.join(&file), body).map_err(|e| e.to_string())?;

    let checkin = if realm_key == "cn" {
        daily_checkin(&access, &uid, &ent, &domain)
    } else {
        String::new()
    };

    Ok(SavedAccount {
        file,
        uid,
        nick,
        checkin,
    })
}

/// /status：账号健康（credits / success_count / cooling …），不含 token 计数
pub fn fetch_status(port: u16, key: &str) -> Result<Value, String> {
    let url = format!("http://127.0.0.1:{}/status", port);
    let agent = ureq::agent();
    let r = agent
        .get(&url)
        .header("Authorization", &format!("Bearer {}", key))
        .header("Accept", "application/json");
    let mut res = r.call().map_err(|e| format!("{}", e))?;
    let text = res.body_mut().read_to_string().map_err(|e| e.to_string())?;
    serde_json::from_str(&text).map_err(|e| e.to_string())
}

pub fn fetch_live_usage(port: u16, key: &str) -> Result<crate::live_hud::LiveUsage, String> {
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(std::time::Duration::from_millis(650)))
        .proxy(None)
        .build().into();
    let mut response = agent.get(format!("http://127.0.0.1:{port}/v1/usage/live"))
        .header("Authorization", &format!("Bearer {key}"))
        .call().map_err(|e| e.to_string())?;
    let body = response.body_mut().read_to_string().map_err(|e| e.to_string())?;
    serde_json::from_str(&body).map_err(|e| e.to_string())
}

#[derive(Clone, Default)]
pub struct ModelRow {
    pub id: String,
    pub name: String,
    pub credits: String,
    pub desc: String,
}

pub fn fetch_models(port: u16, key: &str) -> Result<Vec<ModelRow>, String> {
    let url = format!("http://127.0.0.1:{}/v1/models", port);
    let agent = ureq::agent();
    let r = agent
        .get(&url)
        .header("Authorization", &format!("Bearer {}", key))
        .header("Accept", "application/json");
    let mut res = r.call().map_err(|e| format!("{}", e))?;
    let text = res.body_mut().read_to_string().map_err(|e| e.to_string())?;
    let v: Value = serde_json::from_str(&text).map_err(|e| e.to_string())?;
    let arr = v
        .get("data")
        .and_then(|x| x.as_array())
        .cloned()
        .unwrap_or_default();
    Ok(arr
        .iter()
        .map(|m| ModelRow {
            id: m.get("id").and_then(|x| x.as_str()).unwrap_or("").to_string(),
            name: m.get("name").and_then(|x| x.as_str()).unwrap_or("").to_string(),
            credits: match m.get("credits") {
                Some(Value::String(s)) => s.clone(),
                Some(Value::Number(n)) => n.to_string(),
                _ => "-".to_string(),
            },
            desc: m
                .get("description")
                .and_then(|x| x.as_str())
                .unwrap_or("")
                .replace('|', "/"),
        })
        .collect())
}
