import AVFoundation
import SwiftUI
import UniformTypeIdentifiers

/// First launch: choose how to connect to the encrypted folder.
struct SetupView: View {
    @EnvironmentObject var model: AppModel
    @State private var error: String?
    @State private var working = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 22) {
                    Image(systemName: "lock.rectangle.stack.fill")
                        .font(.system(size: 64)).foregroundStyle(.tint).padding(.top, 36)
                    VStack(spacing: 6) {
                        Text("Crypta").font(.largeTitle.bold())
                        Text("你的 Google Drive 加密网盘，与 rclone crypt 完全兼容。")
                            .font(.subheadline).foregroundStyle(.secondary).multilineTextAlignment(.center)
                    }
                    VStack(spacing: 12) {
                        NavigationLink { PairView() } label: {
                            option("qrcode.viewfinder", "与服务器配对", "在服务器上运行 crypta pair，扫码或输入配对码")
                        }
                        NavigationLink { ImportView() } label: {
                            option("doc.badge.gearshape", "导入 rclone.conf", "直接使用电脑上 rclone 的配置，不需要服务器")
                        }
                        Button {
                            working = true
                            Task {
                                do { try await model.configure("api/setup/demo") } catch let e { error = e.localizedDescription }
                                working = false
                            }
                        } label: {
                            option("play.circle", "先试用演示模式", "文件加密后只保存在本机，不连接 Google Drive")
                        }
                        .disabled(working)
                    }
                    .buttonStyle(.plain)
                    if let error { Text(error).font(.footnote).foregroundStyle(.red) }
                }
                .padding(.horizontal, 20)
            }
        }
    }

    private func option(_ symbol: String, _ title: String, _ detail: String) -> some View {
        HStack(spacing: 14) {
            Image(systemName: symbol).font(.title2).foregroundStyle(.tint).frame(width: 34)
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.headline)
                Text(detail).font(.footnote).foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
            Image(systemName: "chevron.right").font(.footnote).foregroundStyle(.tertiary)
        }
        .padding(14)
        .background(Color(.secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).stroke(Color(.separator).opacity(0.5)))
    }
}

struct PairView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State var server = ""
    @State var code = ""
    var autoStart = false
    @State private var scanning = false
    @State private var working = false
    @State private var error: String?

    var body: some View {
        Form {
            Section {
                Button { scanning = true } label: { Label("扫描二维码", systemImage: "qrcode.viewfinder") }
            } footer: {
                Text("在服务器上运行 docker compose exec core crypta pair，终端会显示二维码和配对码。")
            }
            Section("或手动输入") {
                TextField("服务器地址，如 crypta.example.com", text: $server)
                    .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
                TextField("配对码，如 ABCD-EFGH", text: $code)
                    .textInputAutocapitalization(.characters).autocorrectionDisabled()
            }
            Section {
                Button { pair() } label: {
                    HStack {
                        Text("配对")
                        if working { Spacer(); ProgressView() }
                    }
                }
                .disabled(working || server.trimmingCharacters(in: .whitespaces).isEmpty || code.trimmingCharacters(in: .whitespaces).isEmpty)
            } footer: {
                if let error { Text(error).foregroundStyle(.red) }
            }
        }
        .navigationTitle("与服务器配对")
        .navigationBarTitleDisplayMode(.inline)
        .sheet(isPresented: $scanning) {
            QRScanner { text in
                scanning = false
                if let url = URL(string: text), url.scheme == "crypta",
                   let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems,
                   let s = items.first(where: { $0.name == "server" })?.value,
                   let c = items.first(where: { $0.name == "code" })?.value {
                    server = s
                    code = c
                    pair()
                } else {
                    error = "这不是 Crypta 的配对二维码。"
                }
            }
        }
        .onAppear { if autoStart, !server.isEmpty, !code.isEmpty { pair() } }
    }

    private func pair() {
        working = true
        error = nil
        Task {
            do {
                try await model.configure("api/setup/pair", ["server": server, "code": code, "name": UIDevice.current.name])
                model.pendingPair = nil
                dismiss()
            } catch let e {
                error = e.localizedDescription
            }
            working = false
        }
    }
}

struct ImportView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var conf = ""
    @State private var remotes: [String] = []
    @State private var remote = ""
    @State private var picking = false
    @State private var working = false
    @State private var error: String?

    var body: some View {
        Form {
            Section {
                Button { picking = true } label: { Label("选择 rclone.conf 文件", systemImage: "folder") }
                Button {
                    if let s = UIPasteboard.general.string { conf = s; inspect() }
                } label: { Label("从剪贴板粘贴", systemImage: "doc.on.clipboard") }
            } footer: {
                Text("在电脑上运行 rclone config file 可以看到配置文件的位置。文件里需要有一个建立在 Google Drive 之上的 crypt 远程。配置只保存在这台手机上。")
            }
            if !conf.isEmpty {
                Section("加密远程") {
                    if remotes.isEmpty {
                        Text("这份配置里没有 crypt 远程。").foregroundStyle(.red)
                    } else {
                        Picker("使用", selection: $remote) {
                            ForEach(remotes, id: \.self) { Text($0).tag($0) }
                        }
                    }
                }
                Section {
                    Button { apply() } label: {
                        HStack {
                            Text("连接")
                            if working { Spacer(); ProgressView() }
                        }
                    }
                    .disabled(working || remote.isEmpty)
                } footer: {
                    if let error { Text(error).foregroundStyle(.red) }
                }
            }
        }
        .navigationTitle("导入 rclone.conf")
        .navigationBarTitleDisplayMode(.inline)
        .fileImporter(isPresented: $picking, allowedContentTypes: [.item]) { result in
            guard case .success(let url) = result else { return }
            let scoped = url.startAccessingSecurityScopedResource()
            defer { if scoped { url.stopAccessingSecurityScopedResource() } }
            if let data = try? Data(contentsOf: url), data.count < 4_000_000, let s = String(data: data, encoding: .utf8) {
                conf = s
                inspect()
            } else {
                error = "读不了这个文件。"
            }
        }
    }

    private struct Remotes: Decodable { let remotes: [String]? }

    private func inspect() {
        error = nil
        Task {
            let r: Remotes? = try? await model.api?.post("api/setup/remotes", ["rcloneConf": conf])
            remotes = r?.remotes ?? []
            remote = remotes.first ?? ""
        }
    }

    private func apply() {
        working = true
        error = nil
        Task {
            do {
                try await model.configure("api/setup/rclone", ["rcloneConf": conf, "remote": remote])
                dismiss()
            } catch let e {
                error = e.localizedDescription
            }
            working = false
        }
    }
}

struct SettingsView: View {
    @EnvironmentObject var model: AppModel
    @State private var confirmReset = false
    @State private var confirmUnpair = false

    var body: some View {
        Form {
            Section("存储") {
                LabeledContent("当前", value: model.setup.demo ? "演示模式（仅本机）" : "Google Drive（rclone crypt）")
                NavigationLink { ImportView() } label: { Label("导入 rclone.conf", systemImage: "doc.badge.gearshape") }
            }
            Section {
                if model.setup.server.isEmpty {
                    NavigationLink { PairView() } label: { Label("与服务器配对", systemImage: "qrcode.viewfinder") }
                } else {
                    LabeledContent("地址", value: model.setup.server)
                    NavigationLink { ServerJobsView() } label: { Label("离线下载", systemImage: "icloud.and.arrow.down") }
                    Button("取消配对", role: .destructive) { confirmUnpair = true }
                }
            } header: {
                Text("服务器")
            } footer: {
                Text("服务器是可选的：它负责离线下载，下载完成后加密上传到 Google Drive。")
            }
            Section {
                Button("退出并清除本机配置", role: .destructive) { confirmReset = true }
            } footer: {
                Text("只清除这台手机上的配置，Google Drive 里的文件不会被删除。")
            }
            Section("关于") {
                LabeledContent("版本", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")
                Text("视频播放使用 VLCKit（LGPL 2.1）。").font(.footnote).foregroundStyle(.secondary)
            }
        }
        .navigationTitle("设置")
        .confirmationDialog("清除本机配置？", isPresented: $confirmReset, titleVisibility: .visible) {
            Button("清除", role: .destructive) { Task { try? await model.configure("api/setup/reset") } }
        }
        .confirmationDialog("取消与服务器的配对？", isPresented: $confirmUnpair, titleVisibility: .visible) {
            Button("取消配对", role: .destructive) { Task { try? await model.configure("api/setup/unpair") } }
        } message: {
            Text("如果要让这台手机的令牌失效，请在服务器上运行 crypta revoke。")
        }
    }
}

// MARK: - QR scanner

struct QRScanner: View {
    let onCode: (String) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var denied = false

    var body: some View {
        NavigationStack {
            ZStack {
                Color.black.ignoresSafeArea()
                if denied {
                    VStack(spacing: 10) {
                        Image(systemName: "camera.fill").font(.largeTitle)
                        Text("没有相机权限").font(.headline)
                        Text("请在系统“设置 > Crypta”里允许使用相机，或者手动输入配对码。")
                            .font(.footnote).multilineTextAlignment(.center)
                    }
                    .foregroundStyle(.white).padding(30)
                } else {
                    CameraPreview(onCode: onCode, denied: $denied).ignoresSafeArea()
                    RoundedRectangle(cornerRadius: 18).stroke(.white.opacity(0.9), lineWidth: 3).frame(width: 240, height: 240)
                }
            }
            .navigationTitle("扫描配对二维码")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } } }
            .toolbarBackground(.visible, for: .navigationBar)
        }
    }
}

private struct CameraPreview: UIViewRepresentable {
    let onCode: (String) -> Void
    @Binding var denied: Bool

    final class PreviewView: UIView {
        override class var layerClass: AnyClass { AVCaptureVideoPreviewLayer.self }
        var previewLayer: AVCaptureVideoPreviewLayer { layer as! AVCaptureVideoPreviewLayer }
    }

    final class Coordinator: NSObject, AVCaptureMetadataOutputObjectsDelegate {
        let session = AVCaptureSession()
        var onCode: ((String) -> Void)?
        private var found = false

        func metadataOutput(_ output: AVCaptureMetadataOutput, didOutput objects: [AVMetadataObject], from connection: AVCaptureConnection) {
            guard !found, let s = (objects.first as? AVMetadataMachineReadableCodeObject)?.stringValue else { return }
            found = true
            session.stopRunning()
            onCode?(s)
        }
    }

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeUIView(context: Context) -> PreviewView {
        let view = PreviewView()
        let c = context.coordinator
        c.onCode = onCode
        view.previewLayer.session = c.session
        view.previewLayer.videoGravity = .resizeAspectFill
        AVCaptureDevice.requestAccess(for: .video) { granted in
            DispatchQueue.main.async {
                guard granted, let device = AVCaptureDevice.default(for: .video), let input = try? AVCaptureDeviceInput(device: device), c.session.canAddInput(input) else {
                    denied = true
                    return
                }
                c.session.addInput(input)
                let output = AVCaptureMetadataOutput()
                guard c.session.canAddOutput(output) else { return }
                c.session.addOutput(output)
                output.setMetadataObjectsDelegate(c, queue: .main)
                output.metadataObjectTypes = [.qr]
                DispatchQueue.global(qos: .userInitiated).async { c.session.startRunning() }
            }
        }
        return view
    }

    func updateUIView(_ uiView: PreviewView, context: Context) {}

    static func dismantleUIView(_ uiView: PreviewView, coordinator: Coordinator) {
        coordinator.session.stopRunning()
    }
}
