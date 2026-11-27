import MobileVLCKit
import SwiftUI

struct PreviewView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let item: Item
    @State private var note: String?

    var body: some View {
        NavigationStack {
            Group {
                switch item.kind {
                case .image: ImagePreview(item: item)
                case .text: TextPreview(item: item)
                case .video, .audio: PlayerView(item: item)
                default: fileInfo
                }
            }
            .navigationTitle(item.name)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("关闭") { dismiss() } }
                ToolbarItem(placement: .primaryAction) {
                    Button { download() } label: { Image(systemName: "arrow.down.circle") }
                        .accessibilityLabel("下载到本机")
                }
            }
            .overlay(alignment: .top) {
                if let note {
                    Text(note).font(.footnote)
                        .padding(.horizontal, 14).padding(.vertical, 8)
                        .background(.thinMaterial, in: Capsule())
                        .padding(.top, 8)
                }
            }
        }
    }

    private var fileInfo: some View {
        VStack(spacing: 14) {
            Image(systemName: "doc").font(.system(size: 56)).foregroundStyle(.secondary)
            Text(item.name).font(.headline).multilineTextAlignment(.center)
            Text(formatSize(item.size)).foregroundStyle(.secondary)
            Text("这种文件不能预览，可以下载后用其他 App 打开。").font(.footnote).foregroundStyle(.secondary)
            Button("下载到本机") { download() }.buttonStyle(.borderedProminent)
        }
        .padding()
    }

    private func download() {
        Task {
            do {
                try await model.download(item)
                note = "已加入下载，在“传输”里查看"
            } catch {
                note = error.localizedDescription
            }
            try? await Task.sleep(nanoseconds: 2_500_000_000)
            note = nil
        }
    }
}

private struct LoadFailed: View {
    let message: String
    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: "exclamationmark.triangle").font(.largeTitle).foregroundStyle(.orange)
            Text(message).font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.center)
        }.padding()
    }
}

struct ImagePreview: View {
    @EnvironmentObject var model: AppModel
    let item: Item
    @State private var image: UIImage?
    @State private var failure: String?
    @State private var scale: CGFloat = 1
    @State private var baseScale: CGFloat = 1
    @State private var offset: CGSize = .zero
    @State private var baseOffset: CGSize = .zero

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            if let image {
                Image(uiImage: image)
                    .resizable()
                    .scaledToFit()
                    .scaleEffect(scale)
                    .offset(offset)
                    .gesture(
                        MagnificationGesture()
                            .onChanged { scale = max(1, baseScale * $0) }
                            .onEnded { _ in
                                baseScale = scale
                                if scale == 1 { withAnimation { offset = .zero; baseOffset = .zero } }
                            }
                            .simultaneously(with: DragGesture()
                                .onChanged { v in
                                    guard scale > 1 else { return }
                                    offset = CGSize(width: baseOffset.width + v.translation.width, height: baseOffset.height + v.translation.height)
                                }
                                .onEnded { _ in baseOffset = offset })
                    )
                    .onTapGesture(count: 2) {
                        withAnimation {
                            scale = scale > 1 ? 1 : 2.5
                            baseScale = scale
                            offset = .zero
                            baseOffset = .zero
                        }
                    }
            } else if let failure {
                LoadFailed(message: failure)
            } else {
                ProgressView().tint(.white)
            }
        }
        .task {
            do {
                guard let api = model.api else { return }
                let data = try await api.data(item.path)
                if let img = UIImage(data: data) { image = img } else { failure = "这张图片的格式无法显示。" }
            } catch {
                failure = error.localizedDescription
            }
        }
    }
}

struct TextPreview: View {
    @EnvironmentObject var model: AppModel
    let item: Item
    @State private var text: String?
    @State private var failure: String?
    @State private var truncated = false
    private let limit: Int64 = 1024 * 1024

    var body: some View {
        Group {
            if let text {
                ScrollView {
                    VStack(alignment: .leading, spacing: 12) {
                        Text(text)
                            .font(.system(.footnote, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        if truncated {
                            Text("只显示了前 1 MB，下载后可查看完整内容。").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    .padding()
                }
            } else if let failure {
                LoadFailed(message: failure)
            } else {
                ProgressView()
            }
        }
        .task {
            do {
                guard let api = model.api else { return }
                if item.size == 0 { text = ""; return }
                truncated = item.size > limit
                let data = try await api.data(item.path, range: 0...(min(item.size, limit) - 1))
                text = String(data: data, encoding: .utf8)
                    ?? String(data: data, encoding: String.Encoding(rawValue: CFStringConvertEncodingToNSStringEncoding(CFStringEncoding(CFStringEncodings.GB_18030_2000.rawValue))))
                    ?? String(decoding: data, as: UTF8.self)
            } catch {
                failure = error.localizedDescription
            }
        }
    }
}

// MARK: - Video and audio (VLC)

final class Player: NSObject, ObservableObject, VLCMediaPlayerDelegate {
    let vlc = VLCMediaPlayer()
    @Published var playing = false
    @Published var buffering = true
    @Published var failed = false
    @Published var position: Double = 0
    @Published var elapsed = "00:00"
    @Published var total = "--:--"
    var scrubbing = false

    func open(_ url: URL) {
        let media = VLCMedia(url: url)
        media.addOption(":network-caching=1500")
        vlc.media = media
        vlc.delegate = self
        vlc.play()
    }

    func toggle() { vlc.isPlaying ? vlc.pause() : vlc.play() }
    func skip(_ seconds: Int32) { seconds > 0 ? vlc.jumpForward(seconds) : vlc.jumpBackward(-seconds) }
    func seek(to p: Double) { vlc.position = Float(p) }
    func stop() { vlc.delegate = nil; vlc.stop() }

    func mediaPlayerStateChanged(_ notification: Notification) {
        DispatchQueue.main.async { [self] in
            playing = vlc.isPlaying
            switch vlc.state {
            case .error: failed = true; buffering = false
            case .opening, .buffering: buffering = !vlc.isPlaying
            default: buffering = false
            }
        }
    }

    func mediaPlayerTimeChanged(_ notification: Notification) {
        DispatchQueue.main.async { [self] in
            buffering = false
            playing = vlc.isPlaying
            if !scrubbing { position = Double(vlc.position) }
            elapsed = vlc.time.stringValue
            if let length = vlc.media?.length, length.intValue > 0 { total = length.stringValue }
        }
    }
}

private struct VideoSurface: UIViewRepresentable {
    let player: Player

    func makeUIView(context: Context) -> UIView {
        let view = UIView()
        view.backgroundColor = .black
        player.vlc.drawable = view
        return view
    }

    func updateUIView(_ uiView: UIView, context: Context) {}
}

struct PlayerView: View {
    @EnvironmentObject var model: AppModel
    let item: Item
    @StateObject private var player = Player()
    @State private var controls = true

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            if item.kind == .audio {
                Image(systemName: "music.note").font(.system(size: 72)).foregroundStyle(.white.opacity(0.5))
            }
            VideoSurface(player: player)
                .opacity(item.kind == .audio ? 0 : 1)
                .ignoresSafeArea(edges: .horizontal)
            Color.clear.contentShape(Rectangle())
                .onTapGesture { withAnimation { controls.toggle() } }
            if player.failed {
                LoadFailed(message: "无法播放这个文件。")
            } else if player.buffering {
                ProgressView().tint(.white).scaleEffect(1.3)
            }
            if controls || !player.playing {
                VStack {
                    Spacer()
                    panel
                }
                .transition(.opacity)
            }
        }
        .onAppear {
            if let api = model.api { player.open(api.fileURL(item.path)) }
            UIApplication.shared.isIdleTimerDisabled = true
        }
        .onDisappear {
            player.stop()
            UIApplication.shared.isIdleTimerDisabled = false
        }
    }

    private var panel: some View {
        VStack(spacing: 6) {
            Slider(value: $player.position, in: 0...1) { editing in
                player.scrubbing = editing
                if !editing { player.seek(to: player.position) }
            }
            .accessibilityLabel("进度")
            HStack {
                Text(player.elapsed).monospacedDigit()
                Spacer()
                Text(player.total).monospacedDigit()
            }
            .font(.caption)
            HStack(spacing: 44) {
                Button { player.skip(-10) } label: { Image(systemName: "gobackward.10") }
                    .accessibilityLabel("后退 10 秒")
                Button { player.toggle() } label: {
                    Image(systemName: player.playing ? "pause.fill" : "play.fill").font(.system(size: 34))
                }
                .accessibilityLabel(player.playing ? "暂停" : "播放")
                Button { player.skip(10) } label: { Image(systemName: "goforward.10") }
                    .accessibilityLabel("前进 10 秒")
            }
            .font(.title2)
            .padding(.bottom, 4)
        }
        .foregroundStyle(.white)
        .tint(.white)
        .padding(.horizontal, 20).padding(.vertical, 12)
        .background(.black.opacity(0.55))
    }
}
