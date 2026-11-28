import MobileVLCKit
import SwiftUI

struct PreviewView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let item: Item
    @State private var note: String?
    @State private var showPlayerControls = true

    var body: some View {
        NavigationStack {
            Group {
                switch item.kind {
                case .image: ImagePreview(item: item)
                case .text: TextPreview(item: item)
                case .video, .audio: PlayerView(item: item, controls: $showPlayerControls)
                default: fileInfo
                }
            }
            .navigationTitle(item.name)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar((item.kind != .video || showPlayerControls) ? .visible : .hidden, for: .navigationBar)
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

private func formatClock(_ seconds: Double) -> String {
    let total = max(0, Int(seconds.rounded(.down)))
    let h = total / 3600
    let m = (total % 3600) / 60
    let s = total % 60
    return h > 0 ? String(format: "%d:%02d:%02d", h, m, s) : String(format: "%02d:%02d", m, s)
}

final class Player: NSObject, ObservableObject, VLCMediaPlayerDelegate {
    let vlc = VLCMediaPlayer()
    @Published var playing = false
    @Published var buffering = true
    @Published var failed = false
    @Published var position: Double = 0
    @Published var elapsed = "00:00"
    @Published var total = "--:--"
    @Published var durationSeconds: Double = 0
    @Published var currentSeconds: Double = 0
    var scrubbing = false
    // VLC does not seek reliably while paused, so a seek made then is kept
    // here and applied when playback resumes.
    private var pending: Double?
    // Tracks the target time during a rapid burst of seeks/skips so chained
    // double-taps accumulate accurately before VLC finishes seeking.
    private var seekTargetSeconds: Double?
    private var seekTargetAt: Date = .distantPast

    var effectiveCurrentSeconds: Double {
        if let target = seekTargetSeconds, Date().timeIntervalSince(seekTargetAt) < 1.2 {
            return target
        }
        return currentSeconds
    }

    /// media.splitTracks: the file keeps the audio far from the video of the
    /// same moment, so the audio is read over a second connection. With one,
    /// VLC reconnects on every switch between the tracks and stalls after a
    /// seek.
    func open(_ url: URL, media info: API.Media? = nil) {
        let media = VLCMedia(url: url)
        media.addOption(":network-caching=1500")
        if let info, info.splitTracks {
            media.addOption(":input-slave=\(url.absoluteString)")
            // The first audio track of the second connection.
            media.addOption(":audio-track=\(info.audioTracks)")
        }
        vlc.media = media
        vlc.delegate = self
        vlc.play()
    }

    func toggle() {
        if vlc.isPlaying {
            vlc.pause()
            return
        }
        // VLC ignores play() on a media that has played to its end until it
        // is stopped. It may report the end as .stopped.
        if vlc.state == .ended || vlc.state == .stopped { vlc.stop() }
        vlc.play()
    }

    func skipBy(_ deltaSeconds: Double) {
        if durationSeconds > 0 {
            let target = min(max(effectiveCurrentSeconds + deltaSeconds, 0), durationSeconds)
            seek(toSeconds: target)
        } else {
            let secs = Int32(deltaSeconds)
            if secs > 0 { vlc.jumpForward(secs) } else if secs < 0 { vlc.jumpBackward(-secs) }
        }
    }

    func seek(toSeconds seconds: Double) {
        guard durationSeconds > 0 else { return }
        let clamped = min(max(seconds, 0), durationSeconds)
        seekTargetSeconds = clamped
        seekTargetAt = Date()
        seek(to: clamped / durationSeconds)
    }

    func seek(to p: Double) {
        let clamped = min(max(p, 0), 1)
        if durationSeconds > 0 {
            let sec = durationSeconds * clamped
            seekTargetSeconds = sec
            seekTargetAt = Date()
            currentSeconds = sec
            elapsed = formatClock(sec)
        }
        position = clamped
        if vlc.isPlaying {
            pending = nil
            vlc.position = Float(clamped)
            return
        }
        pending = clamped
    }

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
            if let length = vlc.media?.length, length.intValue > 0 {
                let dur = Double(length.intValue) / 1000.0
                durationSeconds = dur
                total = formatClock(dur)
            }
            if let p = pending {
                guard vlc.isPlaying else { return }
                pending = nil
                vlc.position = Float(p)
                return
            }
            let recentSeek = Date().timeIntervalSince(seekTargetAt) < 0.8
            if !scrubbing && !recentSeek {
                position = Double(vlc.position)
                let cur = max(0, Double(vlc.time.intValue) / 1000.0)
                currentSeconds = cur
                elapsed = formatClock(cur)
                seekTargetSeconds = nil
            }
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

/// Zero-inertia precision scrubber bar: tracks touch 1:1 without spring or deceleration.
private struct SeekBar: View {
    @Binding var value: Double
    let onScrubbing: (Bool, Double) -> Void
    @State private var dragging = false

    var body: some View {
        GeometryReader { geo in
            let width = max(geo.size.width, 1)
            let clamped = min(max(value, 0), 1)
            let fillWidth = width * clamped
            let thumbSize: CGFloat = dragging ? 16 : 12

            ZStack(alignment: .leading) {
                Capsule()
                    .fill(.white.opacity(0.3))
                    .frame(height: 4)
                Capsule()
                    .fill(.white)
                    .frame(width: fillWidth, height: 4)
                Circle()
                    .fill(.white)
                    .frame(width: thumbSize, height: thumbSize)
                    .offset(x: min(max(fillWidth - thumbSize / 2, 0), max(width - thumbSize, 0)))
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .contentShape(Rectangle())
            .gesture(
                DragGesture(minimumDistance: 0)
                    .onChanged { v in
                        let frac = min(max(Double(v.location.x / width), 0), 1)
                        var tx = Transaction()
                        tx.animation = nil
                        withTransaction(tx) {
                            dragging = true
                            value = frac
                        }
                        onScrubbing(true, frac)
                    }
                    .onEnded { v in
                        let frac = min(max(Double(v.location.x / width), 0), 1)
                        var tx = Transaction()
                        tx.animation = nil
                        withTransaction(tx) {
                            value = frac
                            dragging = false
                        }
                        onScrubbing(false, frac)
                    }
            )
        }
        .frame(height: 30)
        .accessibilityLabel("进度")
    }
}

struct PlayerView: View {
    private enum DragAxis { case none, horizontal, vertical }
    private enum TapSide { case left, center, right }

    @EnvironmentObject var model: AppModel
    let item: Item
    @Binding var controls: Bool
    @StateObject private var player = Player()

    // Horizontal swipe-to-seek state
    @State private var dragAxis: DragAxis = .none
    @State private var dragStartSeconds: Double = 0
    @State private var dragTargetSeconds: Double = 0
    @State private var dragDeltaSeconds: Double = 0
    @State private var isHorizontalSeeking = false

    // Chainable double-tap skip state
    @State private var lastTapAt: Date = .distantPast
    @State private var lastTapSide: TapSide = .center
    @State private var singleTapTask: Task<Void, Never>?
    @State private var skipSide: TapSide?
    @State private var skipAccumulated = 0
    @State private var skipHideTask: Task<Void, Never>?

    // Auto-hide controls & live transfer speed
    @State private var controlsHideTask: Task<Void, Never>?
    @State private var streamSpeed: Int64 = 0

    private var showPanel: Bool { controls || !player.playing }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            if item.kind == .audio {
                Image(systemName: "music.note").font(.system(size: 72)).foregroundStyle(.white.opacity(0.5))
            }
            VideoSurface(player: player)
                .opacity(item.kind == .audio ? 0 : 1)
                .ignoresSafeArea(edges: .horizontal)
            gestureLayer
            if isHorizontalSeeking {
                dragSeekHUD
            } else if let side = skipSide, skipAccumulated != 0 {
                skipHUD(side: side, delta: skipAccumulated)
            }
            if player.failed {
                LoadFailed(message: "无法播放这个文件。")
            } else if player.buffering {
                ProgressView().tint(.white).scaleEffect(1.3)
            }
            if showPanel {
                VStack {
                    Spacer()
                    panel
                }
                .transition(.opacity)
            }
        }
        .task {
            guard let api = model.api else { return }
            let info = item.kind == .video ? try? await api.media(item.path) : nil
            if !Task.isCancelled { player.open(api.fileURL(item.path), media: info) }
        }
        .task {
            guard let api = model.api else { return }
            var lastBytes = (try? await api.streamBytes()) ?? 0
            var lastAt = Date()
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 1_000_000_000)
                guard !Task.isCancelled else { break }
                if let nowBytes = try? await api.streamBytes() {
                    let now = Date()
                    let dt = now.timeIntervalSince(lastAt)
                    if dt > 0 {
                        let diff = max(Int64(0), nowBytes - lastBytes)
                        streamSpeed = Int64(Double(diff) / dt)
                    }
                    lastBytes = nowBytes
                    lastAt = now
                }
            }
        }
        .onChange(of: player.playing) { isPlaying in
            if isPlaying {
                scheduleControlsAutoHide()
            } else {
                controlsHideTask?.cancel()
                controls = true
            }
        }
        .onAppear {
            UIApplication.shared.isIdleTimerDisabled = true
            controls = true
        }
        .onDisappear {
            singleTapTask?.cancel()
            skipHideTask?.cancel()
            controlsHideTask?.cancel()
            player.stop()
            UIApplication.shared.isIdleTimerDisabled = false
        }
    }

    private var gestureLayer: some View {
        GeometryReader { geo in
            Color.clear
                .contentShape(Rectangle())
                .gesture(
                    DragGesture(minimumDistance: 0)
                        .onChanged { v in
                            let dx = v.translation.width
                            let dy = v.translation.height
                            if dragAxis == .none {
                                guard hypot(dx, dy) >= 12 else { return }
                                if abs(dx) > abs(dy) * 1.15 {
                                    dragAxis = .horizontal
                                    singleTapTask?.cancel()
                                    skipHideTask?.cancel()
                                    controlsHideTask?.cancel()
                                    skipSide = nil
                                    skipAccumulated = 0
                                    isHorizontalSeeking = true
                                    player.scrubbing = true
                                    dragStartSeconds = player.effectiveCurrentSeconds
                                    dragTargetSeconds = dragStartSeconds
                                    dragDeltaSeconds = 0
                                } else {
                                    dragAxis = .vertical
                                    return
                                }
                            }
                            guard dragAxis == .horizontal else { return }
                            let duration = player.durationSeconds
                            guard duration > 0 else { return }
                            let span = min(max(duration * 0.5, 60), min(duration, 180))
                            let rawDelta = Double(dx / max(geo.size.width, 1)) * span
                            let target = min(max(dragStartSeconds + rawDelta, 0), duration)
                            dragTargetSeconds = target
                            dragDeltaSeconds = target - dragStartSeconds
                            var tx = Transaction()
                            tx.animation = nil
                            withTransaction(tx) {
                                player.position = target / duration
                                player.elapsed = formatClock(target)
                            }
                        }
                        .onEnded { v in
                            let axis = dragAxis
                            dragAxis = .none
                            if axis == .horizontal {
                                isHorizontalSeeking = false
                                player.scrubbing = false
                                if player.durationSeconds > 0 {
                                    player.seek(toSeconds: dragTargetSeconds)
                                }
                                scheduleControlsAutoHide()
                            } else if axis == .none {
                                handleTap(at: v.location.x, width: geo.size.width)
                            }
                        }
                )
        }
    }

    private func handleTap(at x: CGFloat, width: CGFloat) {
        let side: TapSide
        if x < width * 0.42 {
            side = .left
        } else if x > width * 0.58 {
            side = .right
        } else {
            side = .center
        }
        let now = Date()

        // Already in an active left/right skip streak: each additional rapid tap chains +15s / -15s.
        if let activeSide = skipSide, activeSide == side, (side == .left || side == .right) {
            triggerSkip(side: side)
            return
        }

        // Second tap on the same left/right side within 0.30s starts the skip streak.
        if (side == .left || side == .right), lastTapSide == side, now.timeIntervalSince(lastTapAt) < 0.30 {
            singleTapTask?.cancel()
            singleTapTask = nil
            lastTapAt = .distantPast
            skipAccumulated = 0
            triggerSkip(side: side)
            return
        }

        // Single tap candidate.
        lastTapAt = now
        lastTapSide = side
        if skipSide != nil {
            skipHideTask?.cancel()
            skipSide = nil
            skipAccumulated = 0
        }
        singleTapTask?.cancel()
        if side == .center {
            toggleControls()
        } else {
            singleTapTask = Task {
                try? await Task.sleep(nanoseconds: 250_000_000)
                guard !Task.isCancelled else { return }
                await MainActor.run { toggleControls() }
            }
        }
    }

    private func triggerSkip(side: TapSide) {
        let step = side == .right ? 15 : -15
        skipSide = side
        skipAccumulated += step
        player.skipBy(Double(step))
        skipHideTask?.cancel()
        skipHideTask = Task {
            try? await Task.sleep(nanoseconds: 700_000_000)
            guard !Task.isCancelled else { return }
            await MainActor.run {
                withAnimation(.easeOut(duration: 0.2)) {
                    skipSide = nil
                    skipAccumulated = 0
                }
            }
        }
    }

    private func toggleControls() {
        withAnimation(.easeInOut(duration: 0.2)) {
            controls.toggle()
        }
        if controls {
            scheduleControlsAutoHide()
        } else {
            controlsHideTask?.cancel()
        }
    }

    private func scheduleControlsAutoHide() {
        controlsHideTask?.cancel()
        guard player.playing && !player.scrubbing && !isHorizontalSeeking else { return }
        controlsHideTask = Task {
            try? await Task.sleep(nanoseconds: 4_000_000_000)
            guard !Task.isCancelled else { return }
            await MainActor.run {
                if player.playing && !player.scrubbing && !isHorizontalSeeking {
                    withAnimation(.easeInOut(duration: 0.2)) { controls = false }
                }
            }
        }
    }

    private var dragSeekHUD: some View {
        let delta = Int(dragDeltaSeconds.rounded())
        let sign = delta >= 0 ? "+" : ""
        let frac = player.durationSeconds > 0 ? min(max(dragTargetSeconds / player.durationSeconds, 0), 1) : 0
        return VStack(spacing: 8) {
            HStack(spacing: 6) {
                Image(systemName: delta >= 0 ? "forward.fill" : "backward.fill")
                Text("\(formatClock(dragTargetSeconds)) / \(player.total)")
                    .monospacedDigit()
                Text("(\(sign)\(delta)s)")
                    .monospacedDigit()
                    .foregroundStyle(.white.opacity(0.8))
            }
            .font(.subheadline.weight(.semibold))
            GeometryReader { g in
                ZStack(alignment: .leading) {
                    Capsule().fill(.white.opacity(0.3))
                    Capsule().fill(.white).frame(width: g.size.width * frac)
                }
            }
            .frame(width: 150, height: 4)
        }
        .foregroundStyle(.white)
        .padding(.horizontal, 18)
        .padding(.vertical, 12)
        .background(.black.opacity(0.72), in: RoundedRectangle(cornerRadius: 12))
        .allowsHitTesting(false)
    }

    private func skipHUD(side: TapSide, delta: Int) -> some View {
        let forward = delta > 0
        let label = "\(forward ? "+" : "-")\(abs(delta)) 秒"
        return HStack {
            if side == .right { Spacer() }
            VStack(spacing: 6) {
                Image(systemName: forward ? "goforward.15" : "gobackward.15")
                    .font(.title2.weight(.semibold))
                Text(label)
                    .font(.caption.weight(.semibold))
                    .monospacedDigit()
            }
            .foregroundStyle(.white)
            .padding(.horizontal, 20)
            .padding(.vertical, 14)
            .background(.black.opacity(0.65), in: RoundedRectangle(cornerRadius: 14))
            .padding(.horizontal, 28)
            if side == .left { Spacer() }
        }
        .allowsHitTesting(false)
    }

    private var panel: some View {
        VStack(spacing: 6) {
            SeekBar(value: $player.position) { editing, frac in
                player.scrubbing = editing
                if editing {
                    controlsHideTask?.cancel()
                    if player.durationSeconds > 0 {
                        player.elapsed = formatClock(player.durationSeconds * frac)
                    }
                } else {
                    player.seek(to: frac)
                    scheduleControlsAutoHide()
                }
            }
            HStack {
                Text(player.elapsed).monospacedDigit()
                Spacer()
                Text(player.total).monospacedDigit()
            }
            .font(.caption)
            ZStack {
                HStack(spacing: 44) {
                    Button {
                        player.skipBy(-15)
                        scheduleControlsAutoHide()
                    } label: { Image(systemName: "gobackward.15") }
                        .accessibilityLabel("后退 15 秒")
                    Button {
                        player.toggle()
                        scheduleControlsAutoHide()
                    } label: {
                        Image(systemName: player.playing ? "pause.fill" : "play.fill").font(.system(size: 34))
                    }
                    .accessibilityLabel(player.playing ? "暂停" : "播放")
                    Button {
                        player.skipBy(15)
                        scheduleControlsAutoHide()
                    } label: { Image(systemName: "goforward.15") }
                        .accessibilityLabel("前进 15 秒")
                }
                .font(.title2)
                HStack {
                    Spacer()
                    HStack(spacing: 3) {
                        Image(systemName: "arrow.down")
                        Text("\(formatSize(streamSpeed))/s")
                    }
                    .font(.caption2.monospacedDigit())
                    .foregroundStyle(.white.opacity(0.85))
                }
            }
            .padding(.bottom, 4)
        }
        .foregroundStyle(.white)
        .tint(.white)
        .padding(.horizontal, 20).padding(.vertical, 12)
        .background(.black.opacity(0.55))
    }
}
