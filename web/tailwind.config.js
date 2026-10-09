/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        // Cursor/GitHub-dark inspired neutral ramp.
        ink: {
          950: '#0a0d12',
          900: '#0d1117',
          850: '#11161d',
          800: '#161b22',
          750: '#1b222c',
          700: '#212934',
          600: '#2d3745',
          500: '#3b4654',
        },
        line: '#232b36',
        brand: {
          400: '#58a6ff',
          500: '#388bfd',
          600: '#1f6feb',
        },
      },
      fontFamily: {
        sans: ['-apple-system', 'BlinkMacSystemFont', '"PingFang SC"',
          '"Microsoft YaHei"', '"Segoe UI"', 'Roboto', 'sans-serif'],
        mono: ['"SF Mono"', 'Menlo', 'Consolas', '"Liberation Mono"', 'monospace'],
      },
    },
  },
  plugins: [],
}
