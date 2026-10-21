use std::{env, fs, path::PathBuf, process::Command};

fn resource_compiler() -> PathBuf {
    if let Some(rc) = env::var_os("RC") {
        return rc.into();
    }
    if Command::new("rc.exe").arg("/?").output().is_ok() {
        return "rc.exe".into();
    }
    let kits =
        PathBuf::from(env::var_os("ProgramFiles(x86)").expect("Windows SDK location unavailable"))
            .join("Windows Kits/10/bin");
    let mut versions: Vec<_> = fs::read_dir(kits)
        .expect("Install Windows SDK resource compiler")
        .filter_map(Result::ok)
        .map(|entry| entry.path())
        .collect();
    versions.sort();
    versions
        .into_iter()
        .rev()
        .map(|dir| dir.join("x64/rc.exe"))
        .find(|path| path.is_file())
        .expect("Windows SDK x64 rc.exe not found")
}

fn main() {
    println!("cargo:rerun-if-changed=assets/app.rc");
    println!("cargo:rerun-if-changed=assets/WorkBuddy2API.ico");
    println!("cargo:rerun-if-env-changed=RC");
    if !env::var("TARGET")
        .unwrap_or_default()
        .ends_with("windows-msvc")
    {
        return;
    }
    let output = PathBuf::from(env::var_os("OUT_DIR").unwrap()).join("app.res");
    let status = Command::new(resource_compiler())
        .arg("/nologo")
        .arg("/fo")
        .arg(&output)
        .arg("assets/app.rc")
        .status()
        .expect("Failed to execute Windows resource compiler");
    assert!(
        status.success(),
        "Failed to compile application icon resource"
    );
    println!("cargo:rustc-link-arg={}", output.display());
}
