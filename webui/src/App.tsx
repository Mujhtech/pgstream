import { ThemeProvider } from "@/components/theme-provider";
import Migrator from "@/components/migrator";

function App() {
  return (
    <ThemeProvider defaultTheme="dark" storageKey="vite-ui-theme">
      <Migrator />
    </ThemeProvider>
  );
}

export default App;
