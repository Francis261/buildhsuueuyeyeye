import React, { createContext, useContext } from 'react';
import { ViewStyle, TextStyle } from 'react-native';

export const AppColors = {
  primary: '#4F46E5',
  background: '#F8FAFC',
  surface: '#FFFFFF',
  border: '#E2E8F0',
  text: '#0F172A',
  textMuted: '#64748B',
};

const ThemeContext = createContext(AppColors);

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  return (
    <ThemeContext.Provider value={AppColors}>{children}</ThemeContext.Provider>
  );
}

export function useTheme() {
  return useContext(ThemeContext);
}

export const commonStyles = {
  screen: {
    flex: 1,
    backgroundColor: AppColors.background,
    padding: 16,
  } as ViewStyle,
  title: {
    fontSize: 22,
    fontWeight: '700',
    color: AppColors.text,
  } as TextStyle,
  caption: {
    fontSize: 13,
    color: AppColors.textMuted,
  } as TextStyle,
};
