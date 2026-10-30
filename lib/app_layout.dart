import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import 'app_theme.dart';

export 'app_build.dart';

const appVersion = '0.2.184';

ThemeData? _televisionThemeLight;
ThemeData? _televisionThemeDark;

ThemeData televisionTheme(ThemeData theme) {
  if (identical(theme, AppTheme.light)) {
    return _televisionThemeLight ??= _buildTelevisionTheme(theme);
  }
  if (identical(theme, AppTheme.dark)) {
    return _televisionThemeDark ??= _buildTelevisionTheme(theme);
  }
  return _buildTelevisionTheme(theme);
}

ThemeData _buildTelevisionTheme(ThemeData theme) {
  final colors = theme.colorScheme;
  final focusSide = WidgetStateProperty.resolveWith<BorderSide?>(
    (states) => states.contains(WidgetState.focused)
        ? BorderSide(color: colors.primary, width: 3)
        : null,
  );
  final focusBackground = WidgetStateProperty.resolveWith<Color?>(
    (states) =>
        states.contains(WidgetState.focused) ? colors.primaryContainer : null,
  );
  final button = ButtonStyle(
    side: focusSide,
    minimumSize: const WidgetStatePropertyAll(Size(52, 48)),
    textStyle: const WidgetStatePropertyAll(TextStyle(fontSize: 17)),
  );
  return theme.copyWith(
    focusColor: colors.primaryContainer,
    iconButtonTheme: IconButtonThemeData(
      style: button.copyWith(backgroundColor: focusBackground),
    ),
    filledButtonTheme: FilledButtonThemeData(style: button),
    outlinedButtonTheme: OutlinedButtonThemeData(style: button),
    textButtonTheme: TextButtonThemeData(
      style: button.copyWith(backgroundColor: focusBackground),
    ),
    inputDecorationTheme: theme.inputDecorationTheme.copyWith(
      focusedBorder: OutlineInputBorder(
        borderRadius: BorderRadius.circular(16),
        borderSide: BorderSide(color: colors.primary, width: 3),
      ),
    ),
  );
}

class AppDevice {
  const AppDevice({this.television = false, this.version = appVersion});
  final bool television;
  final String version;
  static const channel = MethodChannel('duanju/device');
  static bool get supportsMediaVolume =>
      defaultTargetPlatform == TargetPlatform.iOS;
  static final Stream<double> mediaVolumeChanges =
      const EventChannel('duanju/media_volume').receiveBroadcastStream().map(
        (value) => (value as num).toDouble().clamp(0.0, 1.0),
      );

  static Future<double> getMediaVolume() async {
    final value = await channel.invokeMethod<double>('getMediaVolume');
    if (value == null) throw StateError('系统音量读取失败');
    return value.clamp(0.0, 1.0);
  }

  static Future<void> setMediaVolume(double volume) => channel
      .invokeMethod<void>('setMediaVolume', {'volume': volume.clamp(0.0, 1.0)});

  static Future<double> getBrightness() async {
    if (defaultTargetPlatform != TargetPlatform.android &&
        defaultTargetPlatform != TargetPlatform.iOS) {
      return 0.5;
    }
    try {
      final value = await channel.invokeMethod<double>('getBrightness');
      return (value ?? 0.5).clamp(0.0, 1.0);
    } on PlatformException {
      return 0.5;
    } on MissingPluginException {
      return 0.5;
    }
  }

  static Future<void> setBrightness(double brightness) async {
    if (defaultTargetPlatform != TargetPlatform.android &&
        defaultTargetPlatform != TargetPlatform.iOS) {
      return;
    }
    try {
      await channel.invokeMethod<void>('setBrightness', {
        'brightness': brightness.clamp(0.01, 1.0),
      });
    } on PlatformException {
      return;
    } on MissingPluginException {
      return;
    }
  }

  static Future<void> resetBrightness() async {
    if (defaultTargetPlatform != TargetPlatform.android &&
        defaultTargetPlatform != TargetPlatform.iOS) {
      return;
    }
    try {
      await channel.invokeMethod<void>('resetBrightness');
    } on PlatformException {
      return;
    } on MissingPluginException {
      return;
    }
  }

  static Future<AppDevice> detect({
    AppDevice fallback = const AppDevice(),
  }) async {
    if (defaultTargetPlatform != TargetPlatform.android) {
      return const AppDevice();
    }
    try {
      final data = await channel
          .invokeMapMethod<String, dynamic>('deviceInfo')
          .timeout(const Duration(seconds: 2));
      final television = data?['television'];
      if (television is! bool) return fallback;
      final version = data?['version'];
      return AppDevice(
        television: television,
        version: version is String && version.isNotEmpty
            ? version
            : fallback.version,
      );
    } on PlatformException {
      return fallback;
    } on MissingPluginException {
      return fallback;
    } on TimeoutException {
      return fallback;
    }
  }
}

class AppLayout extends InheritedWidget {
  const AppLayout({
    super.key,
    required this.television,
    this.version = appVersion,
    required super.child,
  });
  final bool television;
  final String version;

  static bool isTelevision(BuildContext context) =>
      context.dependOnInheritedWidgetOfExactType<AppLayout>()?.television ??
      false;
  static String versionOf(BuildContext context) =>
      context.dependOnInheritedWidgetOfExactType<AppLayout>()?.version ??
      appVersion;

  @override
  bool updateShouldNotify(AppLayout oldWidget) =>
      television != oldWidget.television || version != oldWidget.version;
}
